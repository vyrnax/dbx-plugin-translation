package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// 偏好键白名单。
//
// 为什么用白名单而不是任意键：偏好会被前端直接写入，
// 如果不限制键名，前端的一个 bug（例如把整个状态对象当偏好存进来）
// 就可能把几 MB 的垃圾数据塞进库里，而且永远清不掉。
// 白名单让「有哪些偏好」这件事在代码里一目了然。
var prefKeys = map[string]bool{
	// 界面布局
	"sidebarCollapsed": true,
	"lastView":         true,
	// 翻译默认值
	"defaultSourceLang": true,
	"defaultTargetLang": true,
	"defaultEngineId":   true,
	// 即时翻译
	"instantEnabled":    true,
	"instantDebounceMs": true,
	"instantMinChars":   true,
	"instantMaxLength":  true,
	// 流式与分段
	"streamEnabled": true,
	// 标识符自动分词（只影响送引擎的那份数据，原文与历史不动）
	"identifierSplitEnabled": true,
	"autoDirectionEnabled":   true,
	"chunkDelayMs":           true,
	"maxCharsPerReq":         true,
	// 历史
	"historyLimit": true,
	// 【刻意没有】主题与语言的偏好键。
	//
	// 主题、字体、界面语言一律跟随宿主 DBX，不提供任何手动覆盖：
	//   - 用户在 DBX 里切了深色主题或英文界面，期望所有面板一起变；
	//     插件若自顾自保持另一套外观会显得割裂。
	//   - 前端 applyTheme() 会把宿主投递的 --color-* / --font-* / --radius-*
	//     token 应用到文档根元素，i18n 则从 dbxPlugin.locale 取语言，
	//     两者都通过 dbx-plugin-env 事件实时刷新。
	//
	// 因此这两个值没有「持久化」的必要 —— 它们的唯一事实来源是宿主，
	// 存进 prefs 反而会制造「设置里显示 A、界面实际是 B」的不一致。
}

// PrefDefaults 是偏好的默认值。
//
// 这些默认值来自 PLAN.md 第十章「本计划设定的默认值」，
// 是实现时不需要再决策的既定参数。
var PrefDefaults = map[string]any{
	"sidebarCollapsed":       false,
	"lastView":               "translate",
	"defaultSourceLang":      "auto",
	"defaultTargetLang":      "zh",
	"defaultEngineId":        "",
	"instantEnabled":         true,
	"instantDebounceMs":      600, // 范围 200-3000
	"instantMinChars":        2,
	"instantMaxLength":       2000, // 超过则不自动翻译，提示手动点击
	"streamEnabled":          true,
	"identifierSplitEnabled": true, // 默认开：不拆词时机器翻译多半原样吐回
	"autoDirectionEnabled":   true, // 默认开：目标语系与原文撞车时自动换向
	"chunkDelayMs":           1100, // 尊重百度/有道免费档 1 QPS
	"maxCharsPerReq":         20000,
	"historyLimit":           500, // 范围 100-5000
}

// GetPrefs 返回全部偏好，缺失的键用默认值补齐。
//
// 补齐而不是返回部分结果：前端拿到的一定是完整的偏好对象，
// 不需要在每个读取点都写 `prefs.x ?? default`，少一处漏写就少一个 bug。
func (s *Store) GetPrefs() (map[string]any, error) {
	out := map[string]any{}
	for k, v := range PrefDefaults {
		out[k] = v
	}
	rows, err := s.db.Query(`SELECT key, value FROM prefs`)
	if err != nil {
		return nil, fmt.Errorf("查询偏好失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("读取偏好失败: %w", err)
		}
		if !prefKeys[k] {
			continue // 忽略未知键（可能是旧版本遗留）
		}
		var decoded any
		if err := json.Unmarshal([]byte(v), &decoded); err != nil {
			// 单条偏好损坏不应让整个加载失败，用默认值兜底
			continue
		}
		out[k] = decoded
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历偏好失败: %w", err)
	}
	return out, nil
}

// SetPrefs 写入偏好（只接受白名单键），返回写入后的完整偏好与新 rev。
//
// expectRev >= 0 时校验乐观锁。
func (s *Store) SetPrefs(incoming map[string]any, expectRev int) (map[string]any, int, error) {
	clean := sanitizePrefs(incoming)
	var (
		newRev  int
		changed bool
	)
	err := s.WithTx(func(tx *sql.Tx) error {
		if _, err := CheckRevInTx(tx, expectRev); err != nil {
			return err
		}
		for k, v := range clean {
			b, err := json.Marshal(v)
			if err != nil {
				return fmt.Errorf("序列化偏好 %s 失败: %w", k, err)
			}
			if _, err := tx.Exec(`
				INSERT INTO prefs (key, value) VALUES (?,?)
				ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
				k, string(b)); err != nil {
				return fmt.Errorf("写入偏好 %s 失败: %w", k, err)
			}
			changed = true
		}
		if !changed {
			// 没有有效键可写：不递增 rev，直接返回当前值
			cur, err := revInTx(tx)
			if err != nil {
				return err
			}
			newRev = cur
			return nil
		}
		nr, err := bumpRev(tx)
		if err != nil {
			return err
		}
		newRev = nr
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	prefs, err := s.GetPrefs()
	if err != nil {
		return nil, 0, err
	}
	return prefs, newRev, nil
}

// sanitizePrefs 过滤未知键并把数值夹到合法范围。
//
// 夹范围是必要的：前端可能被用户改成极端值（防抖 0ms 会让每次按键都发请求，
// 打爆引擎配额；historyLimit 一百万会让库无限膨胀）。
// 服务端必须自己守住边界，不能信任前端传来的值。
func sanitizePrefs(in map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range in {
		if !prefKeys[k] {
			continue
		}
		switch k {
		case "instantDebounceMs":
			out[k] = clampInt(v, 200, 3000, 600)
		case "instantMinChars":
			out[k] = clampInt(v, 1, 100, 2)
		case "instantMaxLength":
			out[k] = clampInt(v, 100, 100000, 2000)
		case "chunkDelayMs":
			out[k] = clampInt(v, 0, 10000, 1100)
		case "maxCharsPerReq":
			out[k] = clampInt(v, 500, 100000, 20000)
		case "historyLimit":
			out[k] = clampInt(v, 100, 5000, 500)
		case "instantEnabled", "streamEnabled", "sidebarCollapsed", "identifierSplitEnabled", "autoDirectionEnabled":
			if b, ok := v.(bool); ok {
				out[k] = b
			}
		case "lastView":
			// 只接受已知视图名，防止前端写入任意字符串
			if str, ok := v.(string); ok && isKnownView(str) {
				out[k] = str
			}
		case "defaultSourceLang", "defaultTargetLang", "defaultEngineId":
			if str, ok := v.(string); ok {
				out[k] = strings.TrimSpace(str)
			}
		default:
			out[k] = v
		}
	}
	return out
}

func isKnownView(v string) bool {
	switch v {
	case "translate", "engines", "history", "settings":
		return true
	}
	return false
}

// clampInt 把任意 JSON 数值夹到 [lo, hi]，非法值回退为 def。
//
// JSON 反序列化出来的数字统一是 float64，必须处理这个类型；
// 但前端也可能直接传 int（经由 Go 侧构造），所以两种都接。
func clampInt(v any, lo, hi, def int) int {
	var n float64
	switch t := v.(type) {
	case float64:
		n = t
	case int:
		n = float64(t)
	case int64:
		n = float64(t)
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return def
		}
		n = f
	default:
		return def
	}
	// NaN / Inf 必须拦掉，否则比较结果不可预期
	if n != n || n > 1e18 || n < -1e18 {
		return def
	}
	i := int(n)
	if i < lo {
		return lo
	}
	if i > hi {
		return hi
	}
	return i
}
