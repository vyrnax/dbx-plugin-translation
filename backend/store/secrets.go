package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// SaveSecret 保存引擎的密钥密文。
//
// cipher 是 secret.Protector.Protect 的输出（DPAPI 密文 / Keychain 引用令牌 /
// AES-GCM 密文），scheme 记录用的是哪种方案 —— 解密时必须用同一种，
// 否则「换了机器/换了密钥库」时会解不开却不知道为什么。
//
// fingerprint 同步写回 engines 表，这样列表查询不需要 JOIN secrets
// 就能显示「已配置」状态。
func (s *Store) SaveSecret(engineID, cipherText, scheme, fingerprint string) error {
	if strings.TrimSpace(engineID) == "" {
		return errors.New("引擎 ID 不能为空")
	}
	if strings.TrimSpace(cipherText) == "" {
		return errors.New("密文不能为空")
	}
	if strings.TrimSpace(scheme) == "" {
		return errors.New("加密方案标识不能为空")
	}
	return s.WithTx(func(tx *sql.Tx) error {
		// 确认引擎存在：给不存在的引擎存密钥会产生孤儿记录
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM engines WHERE id=?`, engineID).Scan(&exists); err != nil {
			return fmt.Errorf("检查引擎是否存在失败: %w", err)
		}
		if exists == 0 {
			return ErrNotFound
		}
		now := nowISO()
		var existingCreated sql.NullString
		_ = tx.QueryRow(`SELECT created_at FROM secrets WHERE engine_id=?`, engineID).Scan(&existingCreated)
		created := now
		if existingCreated.Valid && existingCreated.String != "" {
			created = existingCreated.String
		}

		if _, err := tx.Exec(`
			INSERT INTO secrets (engine_id, cipher, scheme, created_at, updated_at)
			VALUES (?,?,?,?,?)
			ON CONFLICT(engine_id) DO UPDATE SET
				cipher=excluded.cipher,
				scheme=excluded.scheme,
				updated_at=excluded.updated_at`,
			engineID, cipherText, scheme, created, now); err != nil {
			return fmt.Errorf("写入密钥失败: %w", err)
		}
		// 同步 engines 表的元信息（不递增 rev：密钥变更不影响配置版本，
		// 而且 SaveEngine 通常会在同一批操作里递增）
		if _, err := tx.Exec(`UPDATE engines SET has_secret=1, fingerprint=?, updated_at=? WHERE id=?`,
			fingerprint, now, engineID); err != nil {
			return fmt.Errorf("更新引擎密钥标记失败: %w", err)
		}
		return nil
	})
}

// GetSecret 读取密钥密文与方案标识。不存在时返回 ErrNotFound。
//
// 返回的是【密文】，不是明文 —— 解密由调用方（main.go）用 secret.Protector 完成。
// 这个分层保证了 store 包完全不接触明文密钥。
func (s *Store) GetSecret(engineID string) (SecretRecord, error) {
	var (
		r         SecretRecord
		createdAt sql.NullString
		updatedAt sql.NullString
	)
	err := s.db.QueryRow(`
		SELECT engine_id, cipher, scheme, created_at, updated_at
		FROM secrets WHERE engine_id=?`, engineID).
		Scan(&r.EngineID, &r.Cipher, &r.Scheme, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SecretRecord{}, ErrNotFound
		}
		return SecretRecord{}, fmt.Errorf("读取密钥失败: %w", err)
	}
	r.CreatedAt = createdAt.String
	r.UpdatedAt = updatedAt.String
	return r, nil
}

// DeleteSecret 删除引擎的密钥，并清除 engines 表的标记。
func (s *Store) DeleteSecret(engineID string) error {
	return s.WithTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM secrets WHERE engine_id=?`, engineID); err != nil {
			return fmt.Errorf("删除密钥失败: %w", err)
		}
		if _, err := tx.Exec(`UPDATE engines SET has_secret=0, fingerprint=NULL, updated_at=? WHERE id=?`,
			nowISO(), engineID); err != nil {
			return fmt.Errorf("清除引擎密钥标记失败: %w", err)
		}
		return nil
	})
}

// HasSecret 判断引擎是否已配置密钥。
func (s *Store) HasSecret(engineID string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM secrets WHERE engine_id=?`, engineID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("检查密钥是否存在失败: %w", err)
	}
	return n > 0, nil
}

// CountSecrets 返回密钥记录总数，供诊断与导出统计使用。
func (s *Store) CountSecrets() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM secrets`).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计密钥数量失败: %w", err)
	}
	return n, nil
}
