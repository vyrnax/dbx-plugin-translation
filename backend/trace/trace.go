// Package trace 提供侧车的文件追踪日志。
//
// 为什么必须写文件而不是 stderr：
//
//	DBX 开发宿主在启动侧车后执行 `child.stderr.resume()` —— 也就是【直接丢弃】stderr。
//	桌面端是否展示 stderr 也没有任何保证。因此「装上了但什么都不工作」这类问题，
//	唯一可靠的排查证据就是侧车自己写的日志文件。
//
// 同时 stdout 是【协议通道】：任何写到 stdout 的内容都会污染 JSON Lines 流，
// 宿主解析失败后会直接丢弃侧车，前端表现为「数据永远存不上」且没有任何报错。
// 所以本包绝不写 stdout，也绝不写 stderr（避免被误认为可用）。
//
// 脱敏是强制的：本插件保管用户的 API Key，日志一旦泄露密钥就是安全事故。
// 宿主自己的诊断日志也会脱敏（dev-runtime 的 redact()），但那是宿主侧的保护，
// 不能替代侧车自身的脱敏 —— 因为日志文件是侧车直接写到磁盘的。
package trace

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// maxLogBytes 是日志文件的大小上限，超过则整体丢弃重建。
//
// 为什么用「直接删除重建」而不是滚动分片：侧车日志只用于排查当前问题，
// 历史价值低；单文件 + 截断的实现最简单，也不会让用户的数据目录里堆满分片。
const maxLogBytes = 64 * 1024

var (
	mu   sync.Mutex
	path string
)

// sensitiveKey 匹配需要整体替换为 [REDACTED] 的键名。
//
// 覆盖范围对齐宿主 dev-runtime 的 redact() 正则，并额外加上本项目特有的字段名
// （appSecret / secretKey / accessKeySecret / apiKey 的各种大小写与下划线变体）。
var sensitiveKey = regexp.MustCompile(`(?i)(api[._-]?key|app[._-]?secret|secret[._-]?key|access[._-]?key[._-]?secret|secret|token|password|passwd|authorization|credential|private[._-]?key)`)

// longToken 匹配「疑似密钥」的长串：连续 20 个以上字母数字与 _ + - 的字符。
//
// 这是兜底防线 —— 即使某个键名没被 sensitiveKey 命中，只要值长得像密钥也会被掩码。
//
// 字符类【刻意不含 `/`、`.` 和 `=`】，这是权衡后的决定：
//
//	含 `/` 或 `.` 会把正常 URL 的路径段整体误伤 —— 例如
//	https://api.deepseek.com/v1/chat/completions 里的
//	com/v1/chat/completions（23 字符）会被掩码成 com/****ions。
//	含 `=` 会把 URL 查询串整体误伤 —— 例如
//	?Action=TranslateGeneral 里的 Action=TranslateGeneral（22 字符）
//	会被掩码成 Acti****eral。
//	而 URL 是排查引擎问题时最关键的日志信息，掩掉它得不偿失。
//
//	代价：base64 密钥末尾的 `=` padding 不会被一起掩码。但 padding 本身不含
//	信息量，密钥主体（字母数字，通常远超 20 字符）仍然会被完整掩码，
//	安全性不受影响。
//
//	真正的密钥在日志里总是以 `apiKey=xxx` 这类【带键名】的形态出现，
//	第一步的 kv 正则已经覆盖；本正则只是防止有人直接把裸密钥拼进日志。
var longToken = regexp.MustCompile(`[A-Za-z0-9_+-]{20,}`)

// Init 设置日志文件路径。应在侧车启动时调用一次。
//
// dir 通常是 dataDir()。Init 失败不返回错误 —— 日志不可用绝不能阻止侧车启动，
// 否则会因为「写不了日志」而导致整个插件不可用，这是本末倒置。
func Init(dir string) {
	mu.Lock()
	defer mu.Unlock()
	if dir == "" {
		path = ""
		return
	}
	path = filepath.Join(dir, "sidecar-trace.log")
}

// Log 追加一条带时间戳的日志。format/args 语义同 fmt.Sprintf。
//
// 消息在写盘前会经过 Redact 脱敏。日志文件超过 maxLogBytes 时先删除再写，
// 保证不会无限增长。
func Log(format string, args ...any) {
	mu.Lock()
	p := path
	mu.Unlock()
	if p == "" {
		return
	}

	line := fmt.Sprintf(format, args...)
	line = Redact(line)

	mu.Lock()
	defer mu.Unlock()

	if st, err := os.Stat(p); err == nil && st.Size() > maxLogBytes {
		_ = os.Remove(p)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintf(f, "%s  %s\n", time.Now().Format("2006-01-02 15:04:05.000"), line)
}

// Redact 对任意文本做密钥脱敏，可独立用于错误消息等场景。
//
// 两步处理：
//  1. 键名命中 sensitiveKey 时，把该键后面的值整体替换为 [REDACTED]。
//     同时覆盖 JSON 风格（"apiKey":"xxx"）、表单风格（apiKey=xxx）与
//     Go 结构体打印风格（apiKey:xxx）三种形态。
//  2. 剩余的长串按 longToken 掩码，只保留前 4 后 4 字符。
func Redact(s string) string {
	if s == "" {
		return s
	}

	// 第一步：键名命中则抹掉值。
	// 分组说明：($1) 是键名，中间是分隔符与引号，($2) 是值。
	// 值部分匹配到「引号闭合」或「分隔符/行尾」为止，避免贪婪吞掉后续内容。
	kv := regexp.MustCompile(`(?i)([A-Za-z0-9_.\-]*(?:api[._-]?key|app[._-]?secret|secret[._-]?key|access[._-]?key[._-]?secret|secret|token|password|passwd|authorization|credential|private[._-]?key)[A-Za-z0-9_.\-]*)` +
		`(["\']?\s*[:=]\s*["\']?)([^"\',;&}\]\s]*)`)
	s = kv.ReplaceAllStringFunc(s, func(m string) string {
		sub := kv.FindStringSubmatch(m)
		if len(sub) < 4 {
			return "[REDACTED]"
		}
		return sub[1] + sub[2] + "[REDACTED]"
	})

	// 第二步：兜底掩码剩余长串。
	s = longToken.ReplaceAllStringFunc(s, func(tok string) string {
		if len(tok) <= 8 {
			return tok
		}
		return tok[:4] + "****" + tok[len(tok)-4:]
	})

	return s
}

// Fingerprint 生成密钥的掩码指纹，用于 UI 展示「已配置」而不泄露内容。
//
// 规则：
//   - 空串返回空串（前端据此判断 hasSecret）
//   - 长度 <= 8：全部掩码为 ****（太短时保留任何片段都等于泄露）
//   - 否则保留前 4 后 4，中间用 **** 代替
//
// 注意：前 4 后 4 对 sk- 前缀的 OpenAI 风格 Key 是安全的（前缀是公开信息），
// 但对短 Key 不安全，所以有 <=8 全掩码的分支。
func Fingerprint(secret string) string {
	s := strings.TrimSpace(secret)
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= 8 {
		return "****"
	}
	return string(r[:4]) + "****" + string(r[len(r)-4:])
}
