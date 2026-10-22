package trace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRedact_KeyNames 验证各类键名形态下的值都被抹掉。
// 覆盖 JSON、表单、Go 结构体打印三种风格，以及大小写与下划线/点/横线变体。
func TestRedact_KeyNames(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // 必须出现在结果里
		gone string // 绝不能出现在结果里
	}{
		{"JSON apiKey", `{"apiKey":"sk-abcdef1234567890"}`, `[REDACTED]`, `sk-abcdef1234567890`},
		{"表单 appid+secretKey", `appid=123&secretKey=MySecretValue123`, `[REDACTED]`, `MySecretValue123`},
		{"Go 打印 password", `password: hunter2xxx`, `[REDACTED]`, `hunter2xxx`},
		{"大写 Authorization", `Authorization: Bearer eyJhbGciOiJIUzI1NiJ9`, `[REDACTED]`, `eyJhbGciOiJIUzI1NiJ9`},
		{"下划线 api_key", `api_key=AKIAIOSFODNN7EXAMPLE`, `[REDACTED]`, `AKIAIOSFODNN7EXAMPLE`},
		{"点号 access.key.secret", `access.key.secret=verySecretValue`, `[REDACTED]`, `verySecretValue`},
		{"token", `token=abc123def456ghi789`, `[REDACTED]`, `abc123def456ghi789`},
		{"appSecret 驼峰", `appSecret: TopSecret999`, `[REDACTED]`, `TopSecret999`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Redact(c.in)
			if strings.Contains(got, c.gone) {
				t.Errorf("密钥泄露！\n  输入: %s\n  输出: %s\n  仍含: %s", c.in, got, c.gone)
			}
			if !strings.Contains(got, c.want) {
				t.Errorf("未替换为 %s\n  输入: %s\n  输出: %s", c.want, c.in, got)
			}
		})
	}
}

// TestRedact_LongTokenFallback 验证键名未命中时的长串兜底掩码。
//
// 注意字符类包含 `=`，因此 "blob=VALUE" 会被当作【一个】token 整体匹配，
// 掩码保留的是整个 token 的前 4 后 4（即 "blob" 与值的末 4 位）。
// 这是可接受的：兜底掩码的目标是「不让完整密钥出现在日志里」，
// 而不是「精确保留键名」。
func TestRedact_LongTokenFallback(t *testing.T) {
	in := "blob=ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	got := Redact(in)
	if strings.Contains(got, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		t.Errorf("长串未被掩码: %s", got)
	}
	if !strings.Contains(got, "****") {
		t.Errorf("缺少掩码标记: %s", got)
	}
	// 值的中段绝不能出现（这是安全断言，比"保留前4后4"更重要）
	if strings.Contains(got, "EFGHIJKLMNOP") {
		t.Errorf("掩码泄露了中段: %s", got)
	}

	// 用空格分隔时，只有值本身被匹配，此时应保留值的前4后4
	in2 := "credential blob ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	got2 := Redact(in2)
	if strings.Contains(got2, "EFGHIJKLMNOP") {
		t.Errorf("空格分隔场景泄露中段: %s", got2)
	}
}

// TestRedact_URLNotMasked 是移除字符类中 `/` 与 `.` 的回归测试。
//
// 背景：字符类原本含 `/`，导致正常 URL 的路径段被整体掩码 ——
// `https://api.deepseek.com/v1/chat/completions` 里的
// `com/v1/chat/completions`（23 字符）会变成 `com/****ions`。
// 而 URL 是排查引擎问题时最关键的日志信息，掩掉它得不偿失。
func TestRedact_URLNotMasked(t *testing.T) {
	cases := []string{
		"https://api.deepseek.com/v1/chat/completions",
		"endpoint=https://api-free.deepl.com/v2/translate",
		"POST https://fanyi-api.baidu.com/api/trans/vip/translate failed",
		"https://mt.cn-hangzhou.aliyuncs.com/?Action=TranslateGeneral",
		"数据目录 C:\\Users\\me\\AppData\\Roaming\\dbx\\plugins",
	}
	for _, in := range cases {
		got := Redact(in)
		if strings.Contains(got, "****") {
			t.Errorf("URL/路径被误伤掩码:\n  输入: %s\n  输出: %s", in, got)
		}
	}
}

// TestRedact_URLWithSecretKeyStillMasked 验证「URL 不被掩码」不会放过真正的密钥：
// 密钥总是以带键名的形态出现，第一步 kv 正则已经覆盖。
func TestRedact_URLWithSecretKeyStillMasked(t *testing.T) {
	in := "url=https://api.example.com/v1?key=SecretKeyValue123456789"
	got := Redact(in)
	if strings.Contains(got, "SecretKeyValue123456789") {
		t.Errorf("URL 查询串里的密钥泄露: %s", got)
	}
}

// TestRedact_NormalTextUntouched 验证正常文本（URL、路径、中文）不被误伤。
func TestRedact_NormalTextUntouched(t *testing.T) {
	cases := []string{
		"translate/start ok taskId=12 elapsed=340ms",
		"数据目录 C:\\Users\\me\\AppData\\dbx",
		"端点 https://api.deepseek.com/v1/chat/completions",
		"引擎数量 3，默认引擎 openai",
		"connection refused after 60s",
	}
	for _, in := range cases {
		got := Redact(in)
		// URL 里的路径段可能触发长串掩码，这里只断言关键语义片段仍在
		if strings.Contains(in, "taskId=12") && !strings.Contains(got, "taskId=12") {
			t.Errorf("正常文本被误伤:\n  输入: %s\n  输出: %s", in, got)
		}
		if strings.Contains(in, "引擎数量") && !strings.Contains(got, "引擎数量") {
			t.Errorf("中文被误伤:\n  输入: %s\n  输出: %s", in, got)
		}
	}
}

// TestRedact_Empty 验证空输入不 panic。
func TestRedact_Empty(t *testing.T) {
	if got := Redact(""); got != "" {
		t.Errorf("空串应返回空串，得到 %q", got)
	}
}

// TestFingerprint 验证指纹掩码规则。
func TestFingerprint(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"short", "****"},
		{"12345678", "****"},          // 恰好 8 位，全掩码
		{"123456789", "1234****6789"}, // 9 位，保留前4后4
		{"sk-abcdefghijklmnop", "sk-a****mnop"},
		{"中文密钥测试字符串", "中文密钥****试字符串"}, // 9 个 rune：前4 + **** + 后4（按 rune 计数而非字节）
	}
	for _, c := range cases {
		if got := Fingerprint(c.in); got != c.want {
			t.Errorf("Fingerprint(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// TestFingerprint_NoLeak 验证指纹不包含完整密钥。
func TestFingerprint_NoLeak(t *testing.T) {
	secret := "sk-proj-ABCDEFGHIJKLMNOPQRSTUVWX"
	fp := Fingerprint(secret)
	if strings.Contains(fp, secret) {
		t.Errorf("指纹泄露了完整密钥: %s", fp)
	}
	// 中间部分绝不能出现
	if strings.Contains(fp, "EFGHIJKLMNOP") {
		t.Errorf("指纹泄露了密钥中段: %s", fp)
	}
}

// TestLog_NoPathNoPanic 验证未 Init 时 Log 不 panic（日志不可用绝不能拖垮侧车）。
func TestLog_NoPathNoPanic(t *testing.T) {
	mu.Lock()
	path = ""
	mu.Unlock()
	Log("这条日志不应导致 panic: %d", 42)
}

// TestLog_WritesRedacted 验证写盘内容已脱敏。
func TestLog_WritesRedacted(t *testing.T) {
	dir := t.TempDir()
	Init(dir)
	defer Init("")

	const secret = "SuperSecretKeyValue123456"
	Log("engines/save kind=%s apiKey=%s", "openai", secret)

	b, err := os.ReadFile(filepath.Join(dir, "sidecar-trace.log"))
	if err != nil {
		t.Fatalf("读日志失败: %v", err)
	}
	if strings.Contains(string(b), secret) {
		t.Errorf("日志文件泄露密钥！\n内容: %s", string(b))
	}
	if !strings.Contains(string(b), "[REDACTED]") {
		t.Errorf("日志未包含脱敏标记:\n内容: %s", string(b))
	}
	if !strings.Contains(string(b), "engines/save") {
		t.Errorf("日志丢失了正常内容:\n内容: %s", string(b))
	}
}

// TestLog_Truncate 验证超过 64 KiB 后截断重建。
func TestLog_Truncate(t *testing.T) {
	dir := t.TempDir()
	Init(dir)
	defer Init("")

	p := filepath.Join(dir, "sidecar-trace.log")
	// 先写一个超过上限的文件
	if err := os.WriteFile(p, []byte(strings.Repeat("x", maxLogBytes+100)), 0o600); err != nil {
		t.Fatalf("准备失败: %v", err)
	}
	Log("触发截断")

	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat 失败: %v", err)
	}
	if st.Size() > 1024 {
		t.Errorf("日志未截断，当前大小 %d", st.Size())
	}
}
