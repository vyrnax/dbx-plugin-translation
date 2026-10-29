package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// BuiltinEngineID 是内置引擎在 engines 表里的固定 ID。
//
// 为什么必须固定而不是每次生成：内置引擎要能被【稳定识别】——
// 存储层的删除保护、前端的「隐藏编辑/删除」、翻译页的免费服务提示，
// 都靠 builtin 列 + 这个 ID 共同确定；固定 ID 还保证升级后
// EnsureBuiltin 命中的是同一条记录，不会累积出第二条内置引擎。
const BuiltinEngineID = "builtin.free"

// ErrBuiltin 表示操作命中了「内置引擎不可改动」这条规则。
//
// 单独一个哨兵错误（而不是复用 ErrNotFound 或返回通用文本），
// 是为了让 RPC 层能给出可操作的文案：
// 「不可删除，可以禁用」与「记录不存在」是完全不同的下一步。
var ErrBuiltin = errors.New("内置引擎不可删除或改写")

// EnsureBuiltin 保证内置引擎记录存在且与代码定义一致（幂等）。
//
// tpl 是代码侧的内置引擎模板（ID / Name / Kind / Config / Enabled / Builtin）。
//
// 三种情况：
//  1. 库里没有这条记录 → 插入。若此时还没有任何【用户】引擎，
//     内置引擎直接当默认，这样「装好插件第一次打开就能翻译」成立；
//     否则排到末尾并保持非默认，绝不抢走用户已经选定的默认引擎。
//  2. 记录已存在但 name/kind/config 与代码不一致 → 刷新这三列。
//     为什么必须刷：升级换了模型或端点时，旧记录里的常量会把插件
//     永远锁在旧服务端上；而 enabled / is_default / ord 属于用户，一个都不动。
//  3. 记录已存在且完全一致 → 【什么都不写】。
//     这点是刻意的：启动时若无脑 UPDATE，每次启动都会 bumpRev，
//     另一个 DBX 窗口手里的 rev 立刻过期，用户会看到莫名其妙的
//     「配置已被其它会话修改」冲突重试。所以只在真的有差异时才写。
//
// 另外兜底一条：若因任何原因（例如用户删掉了原默认引擎）导致
// 没有任何默认引擎，把内置引擎提升为默认，保证翻译页总有可用引擎。
//
// 返回 created=true 仅表示本次首次创建（供启动日志区分新装与升级）。
func (s *Store) EnsureBuiltin(tpl Engine) (created bool, err error) {
	if tpl.ID == "" {
		return false, errors.New("内置引擎模板缺少 ID")
	}
	tpl.Builtin = true
	cfgJSON, err := marshalConfig(tpl.Config)
	if err != nil {
		return false, err
	}

	err = s.WithTx(func(tx *sql.Tx) error {
		var (
			curName, curKind, curCfg string
			curEnabled, curDefault   int
			found                    bool
		)
		row := tx.QueryRow(
			`SELECT name, kind, config_json, enabled, is_default FROM engines WHERE id=?`, tpl.ID)
		switch scanErr := row.Scan(&curName, &curKind, &curCfg, &curEnabled, &curDefault); scanErr {
		case nil:
			found = true
		case sql.ErrNoRows:
			found = false
		default:
			return fmt.Errorf("查询内置引擎记录失败: %w", scanErr)
		}

		changed := false
		if !found {
			// 首次创建：没有任何用户引擎时直接当默认。
			var userCount int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM engines WHERE builtin=0`).Scan(&userCount); err != nil {
				return fmt.Errorf("统计用户引擎数量失败: %w", err)
			}
			isDefault := userCount == 0

			ord := 0
			if !isDefault {
				// 非默认时排到末尾：内置引擎插在用户自己排好的顺序中间，
				// 会让用户觉得「拖了个内置的进来捣乱」。
				var maxOrd sql.NullInt64
				if err := tx.QueryRow(`SELECT MAX(ord) FROM engines`).Scan(&maxOrd); err != nil {
					return fmt.Errorf("读取最大排序值失败: %w", err)
				}
				ord = int(maxOrd.Int64) + 1
			}

			now := nowISO()
			if _, err := tx.Exec(`
				INSERT INTO engines (id, name, kind, config_json, has_secret, fingerprint,
				                     is_default, enabled, ord, builtin, created_at, updated_at)
				VALUES (?,?,?,?,0,'',?,?,?,1,?,?)`,
				tpl.ID, tpl.Name, tpl.Kind, cfgJSON,
				boolToInt(isDefault), boolToInt(tpl.Enabled), ord, now, now); err != nil {
				return fmt.Errorf("写入内置引擎失败: %w", err)
			}
			created = true
			changed = true
		} else if curName != tpl.Name || curKind != tpl.Kind || curCfg != cfgJSON {
			// 只刷新【代码拥有】的三列。enabled / is_default / ord 属于用户。
			if _, err := tx.Exec(
				`UPDATE engines SET name=?, kind=?, config_json=?, builtin=1, updated_at=? WHERE id=?`,
				tpl.Name, tpl.Kind, cfgJSON, nowISO(), tpl.ID); err != nil {
				return fmt.Errorf("刷新内置引擎定义失败: %w", err)
			}
			changed = true
		}

		// 兜底：没有默认引擎（可能刚被删掉）时提升内置引擎。
		var defCount int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM engines WHERE is_default=1`).Scan(&defCount); err != nil {
			return fmt.Errorf("统计默认引擎数量失败: %w", err)
		}
		if defCount == 0 {
			if _, err := tx.Exec(`UPDATE engines SET is_default=1 WHERE id=?`, tpl.ID); err != nil {
				return fmt.Errorf("回退默认引擎失败: %w", err)
			}
			changed = true
		}

		// 只有真的写过才递增 rev —— 见函数头第 3 种情况的说明。
		if changed {
			if _, err := bumpRev(tx); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return created, nil
}
