package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ListEngines 返回全部引擎：默认引擎永远置顶，其余按 ord 升序、创建时间升序排列。
//
// 「默认置顶」是产品硬性要求：翻译页的引擎下拉顺序必须与引擎列表完全一致，
// 且默认引擎必须排在第一个。否则用户改了默认引擎后，下拉列表仍把旧引擎排在前面，
// 「记住上次选择」的恢复逻辑也会和用户看到的顺序打架。
//
// 【刻意不 JOIN secrets 表】：列表查询根本不需要密文，
// 不碰它就从根本上杜绝了「不小心把密钥查出来」的可能。
// HasSecret 与 Fingerprint 存在 engines 表里，是脱敏后的元信息。
func (s *Store) ListEngines() ([]Engine, error) {
	rows, err := s.db.Query(`
		SELECT id, name, kind, config_json, has_secret, fingerprint,
		       is_default, enabled, ord, created_at, updated_at
		FROM engines
		ORDER BY is_default DESC, ord ASC, created_at ASC, rowid ASC`)
	if err != nil {
		return nil, fmt.Errorf("查询引擎列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Engine, 0, 8)
	for rows.Next() {
		e, err := scanEngine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历引擎列表失败: %w", err)
	}
	return out, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanEngine(r scanner) (Engine, error) {
	var (
		e           Engine
		configJSON  string
		hasSecret   int
		fingerprint sql.NullString
		isDefault   int
		enabled     int
		createdAt   sql.NullString
		updatedAt   sql.NullString
	)
	if err := r.Scan(&e.ID, &e.Name, &e.Kind, &configJSON, &hasSecret, &fingerprint,
		&isDefault, &enabled, &e.Ord, &createdAt, &updatedAt); err != nil {
		return Engine{}, fmt.Errorf("读取引擎记录失败: %w", err)
	}
	e.Config = unmarshalConfig(configJSON)
	e.HasSecret = hasSecret != 0
	e.Fingerprint = fingerprint.String
	e.IsDefault = isDefault != 0
	e.Enabled = enabled != 0
	e.CreatedAt = createdAt.String
	e.UpdatedAt = updatedAt.String
	return e, nil
}

// GetEngine 按 ID 读取单个引擎。不存在时返回 ErrNotFound。
func (s *Store) GetEngine(id string) (Engine, error) {
	row := s.db.QueryRow(`
		SELECT id, name, kind, config_json, has_secret, fingerprint,
		       is_default, enabled, ord, created_at, updated_at
		FROM engines WHERE id=?`, id)
	e, err := scanEngine(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Engine{}, ErrNotFound
		}
		// scanEngine 包装了错误，需要再判一次底层
		if strings.Contains(err.Error(), "no rows") {
			return Engine{}, ErrNotFound
		}
		return Engine{}, err
	}
	return e, nil
}

// SaveEngine 新增或更新引擎（不含密钥）。
//
// expectRev >= 0 时校验乐观锁；冲突返回 ErrConflict 与当前 rev。
// 返回新的 rev。
//
// 密钥由 SaveSecret 单独处理 —— 这个分离是刻意的：
// 更新引擎名称/配置时不应该要求重新输入 API Key，
// 而更新 Key 时也不应该动其他字段。
func (s *Store) SaveEngine(e Engine, expectRev int) (int, error) {
	if strings.TrimSpace(e.ID) == "" {
		return 0, errors.New("引擎 ID 不能为空")
	}
	if strings.TrimSpace(e.Kind) == "" {
		return 0, errors.New("引擎类型不能为空")
	}
	if strings.TrimSpace(e.Name) == "" {
		e.Name = e.Kind // 名称留空时用类型兜底，避免列表里出现空白行
	}

	var newRev int
	err := s.WithTx(func(tx *sql.Tx) error {
		if _, err := CheckRevInTx(tx, expectRev); err != nil {
			return err
		}
		cfgJSON, err := marshalConfig(e.Config)
		if err != nil {
			return err
		}
		now := nowISO()

		// 判断是新增还是更新：新增要设 created_at，更新要保留原值
		var existingCreated sql.NullString
		_ = tx.QueryRow(`SELECT created_at FROM engines WHERE id=?`, e.ID).Scan(&existingCreated)
		created := now
		if existingCreated.Valid && existingCreated.String != "" {
			created = existingCreated.String
		}

		if _, err := tx.Exec(`
			INSERT INTO engines (id, name, kind, config_json, has_secret, fingerprint,
			                     is_default, enabled, ord, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET
				name=excluded.name,
				kind=excluded.kind,
				config_json=excluded.config_json,
				is_default=excluded.is_default,
				enabled=excluded.enabled,
				ord=excluded.ord,
				updated_at=excluded.updated_at`,
			e.ID, e.Name, e.Kind, cfgJSON, boolToInt(e.HasSecret), e.Fingerprint,
			boolToInt(e.IsDefault), boolToInt(e.Enabled), e.Ord, created, now); err != nil {
			return fmt.Errorf("写入引擎失败: %w", err)
		}

		// 设为默认时，必须清掉其他引擎的默认标记 ——
		// 否则会出现两个「默认引擎」，翻译时不知道用哪个。
		if e.IsDefault {
			if _, err := tx.Exec(`UPDATE engines SET is_default=0 WHERE id<>?`, e.ID); err != nil {
				return fmt.Errorf("清除旧默认引擎失败: %w", err)
			}
		}
		// 如果这是第一个引擎，自动设为默认（用户不必手动点一次）
		var count int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM engines`).Scan(&count); err != nil {
			return fmt.Errorf("统计引擎数量失败: %w", err)
		}
		if count == 1 {
			if _, err := tx.Exec(`UPDATE engines SET is_default=1 WHERE id=?`, e.ID); err != nil {
				return fmt.Errorf("设置首个引擎为默认失败: %w", err)
			}
		}

		nr, err := bumpRev(tx)
		if err != nil {
			return err
		}
		newRev = nr
		return nil
	})
	if err != nil {
		return 0, err
	}
	return newRev, nil
}

// CheckRevInTx 是 CheckRev 的包级版本，供 WithTx 回调内使用。
func CheckRevInTx(tx *sql.Tx, expectRev int) (int, error) {
	return revCheck(tx, expectRev)
}

func revCheck(tx *sql.Tx, expectRev int) (int, error) {
	cur, err := revInTx(tx)
	if err != nil {
		return 0, err
	}
	if expectRev >= 0 && expectRev != cur {
		return cur, ErrConflict
	}
	return cur, nil
}

// DeleteEngine 删除引擎及其密钥。
//
// 密钥必须一起删 —— 否则 secrets 表里会留下孤儿记录，
// 既占空间，也可能在引擎 ID 复用时被错误地关联上。
func (s *Store) DeleteEngine(id string, expectRev int) (int, error) {
	var newRev int
	err := s.WithTx(func(tx *sql.Tx) error {
		if _, err := CheckRevInTx(tx, expectRev); err != nil {
			return err
		}
		// 先确认存在，避免「删了个不存在的 ID」也递增 rev
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM engines WHERE id=?`, id).Scan(&exists); err != nil {
			return fmt.Errorf("检查引擎是否存在失败: %w", err)
		}
		if exists == 0 {
			return ErrNotFound
		}
		if _, err := tx.Exec(`DELETE FROM secrets WHERE engine_id=?`, id); err != nil {
			return fmt.Errorf("删除密钥失败: %w", err)
		}
		if _, err := tx.Exec(`DELETE FROM engines WHERE id=?`, id); err != nil {
			return fmt.Errorf("删除引擎失败: %w", err)
		}
		// 删掉的是默认引擎时，把剩下的第一个提升为默认，
		// 否则用户会发现「没有默认引擎了」，翻译按钮不知道用哪个。
		var defCount int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM engines WHERE is_default=1`).Scan(&defCount); err != nil {
			return fmt.Errorf("统计默认引擎失败: %w", err)
		}
		if defCount == 0 {
			if _, err := tx.Exec(`
				UPDATE engines SET is_default=1
				WHERE id=(SELECT id FROM engines ORDER BY ord ASC, created_at ASC, rowid ASC LIMIT 1)`); err != nil {
				return fmt.Errorf("提升新默认引擎失败: %w", err)
			}
		}
		nr, err := bumpRev(tx)
		if err != nil {
			return err
		}
		newRev = nr
		return nil
	})
	if err != nil {
		return 0, err
	}
	return newRev, nil
}

// SetDefaultEngine 把指定引擎设为默认。
func (s *Store) SetDefaultEngine(id string, expectRev int) (int, error) {
	var newRev int
	err := s.WithTx(func(tx *sql.Tx) error {
		if _, err := CheckRevInTx(tx, expectRev); err != nil {
			return err
		}
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM engines WHERE id=?`, id).Scan(&exists); err != nil {
			return fmt.Errorf("检查引擎是否存在失败: %w", err)
		}
		if exists == 0 {
			return ErrNotFound
		}
		// 单事务内先全清再置位，保证任何时刻只有一个默认引擎
		if _, err := tx.Exec(`UPDATE engines SET is_default=0`); err != nil {
			return fmt.Errorf("清除默认标记失败: %w", err)
		}
		if _, err := tx.Exec(`UPDATE engines SET is_default=1, updated_at=? WHERE id=?`, nowISO(), id); err != nil {
			return fmt.Errorf("设置默认引擎失败: %w", err)
		}
		nr, err := bumpRev(tx)
		if err != nil {
			return err
		}
		newRev = nr
		return nil
	})
	if err != nil {
		return 0, err
	}
	return newRev, nil
}

// SetEngineEnabled 启用/禁用引擎。
//
// 禁用而非删除：用户可能只是暂时不想用某个引擎（例如配额用完了），
// 保留配置比让他重新填一遍 API Key 友好得多。
func (s *Store) SetEngineEnabled(id string, enabled bool, expectRev int) (int, error) {
	var newRev int
	err := s.WithTx(func(tx *sql.Tx) error {
		if _, err := CheckRevInTx(tx, expectRev); err != nil {
			return err
		}
		res, err := tx.Exec(`UPDATE engines SET enabled=?, updated_at=? WHERE id=?`,
			boolToInt(enabled), nowISO(), id)
		if err != nil {
			return fmt.Errorf("更新引擎状态失败: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		nr, err := bumpRev(tx)
		if err != nil {
			return err
		}
		newRev = nr
		return nil
	})
	if err != nil {
		return 0, err
	}
	return newRev, nil
}

// ReorderEngines 按给定 ID 顺序重排 ord。
//
// 不在列表里的引擎保持原有相对顺序排在后面（用一个大偏移量），
// 这样「拖拽排序时漏了某个引擎」不会导致它消失。
func (s *Store) ReorderEngines(ids []string, expectRev int) (int, error) {
	var newRev int
	err := s.WithTx(func(tx *sql.Tx) error {
		if _, err := CheckRevInTx(tx, expectRev); err != nil {
			return err
		}
		now := nowISO()
		for i, id := range ids {
			if strings.TrimSpace(id) == "" {
				continue
			}
			if _, err := tx.Exec(`UPDATE engines SET ord=?, updated_at=? WHERE id=?`, i, now, id); err != nil {
				return fmt.Errorf("重排引擎失败: %w", err)
			}
		}
		// 未出现在列表里的引擎排到后面。
		//
		// 参数顺序必须与 SQL 里占位符的出现顺序一致：
		// `ord = ord + ?` 的 offset 在前，`NOT IN (?,?,...)` 的 id 列表在后。
		// 写反了会让 offset 被当成 id 比较，导致所有引擎都被加上一个错误的偏移。
		if len(ids) > 0 {
			// 参数顺序必须严格对应 SQL 里占位符的出现顺序：
			//   SET ord = ord + ?   → offset
			//   SET updated_at = ?  → now
			//   WHERE id NOT IN (?) → ids...
			// 之前把 now 放在末尾，导致 updated_at 被写成第一个 id、
			// NOT IN 比较的是「第二个 id 和时间戳」，排序完全失效。
			args := make([]any, 0, len(ids)+2)
			args = append(args, len(ids)) // offset
			args = append(args, now)      // updated_at
			args = append(args, argsOf(ids)...)
			if _, err := tx.Exec(`
				UPDATE engines SET ord = ord + ?, updated_at = ?
				WHERE id NOT IN (`+placeholders(len(ids))+`)`,
				args...); err != nil {
				return fmt.Errorf("重排剩余引擎失败: %w", err)
			}
		}
		nr, err := bumpRev(tx)
		if err != nil {
			return err
		}
		newRev = nr
		return nil
	})
	if err != nil {
		return 0, err
	}
	return newRev, nil
}

// DefaultEngine 返回默认引擎；没有默认时返回第一个启用的引擎。
//
// 这个回退很重要：用户可能删掉了默认引擎，或者从未设过默认，
// 此时「翻译」按钮不应该直接报错，而应该用一个合理的引擎。
func (s *Store) DefaultEngine() (Engine, error) {
	engines, err := s.ListEngines()
	if err != nil {
		return Engine{}, err
	}
	for _, e := range engines {
		if e.IsDefault && e.Enabled {
			return e, nil
		}
	}
	// 默认引擎被禁用了 → 找第一个启用的
	for _, e := range engines {
		if e.Enabled {
			return e, nil
		}
	}
	return Engine{}, ErrNotFound
}

func placeholders(n int) string {
	if n <= 0 {
		return "''"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func argsOf(ids []string) []any {
	out := make([]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, id)
	}
	return out
}
