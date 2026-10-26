package engine

// 本文件提供所有引擎适配器共享的通用逻辑。
//
// 为什么必须抽出来：8 个适配器如果各自实现「网络错误分类」「译文清洗」
// 「请求校验」，就会有 8 份逐渐漂移的副本 —— 例如某个适配器忘了把 429
// 归类为限流而去重试，结果触发服务商封禁。集中一处后，改一次全改到。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// 请求校验
// ---------------------------------------------------------------------------

// ValidateRequest 校验翻译请求的通用约束。
//
// 所有适配器都应在 Translate/Stream 开头调用它。集中校验的好处是
// 「目标语言不能是 auto」这类规则只需写一遍 —— 分散到 8 个适配器里
// 必然有漏写的，而漏写的症状是上游返回一个看不懂的错误码。
// ValidateRequest 校验翻译请求的通用约束。
//
// 所有适配器都应在 Translate/Stream 开头调用它。集中校验的好处是
// 「目标语言不能是 auto」这类规则只需写一遍 —— 分散到 8 个适配器里
// 必然有漏写的，而漏写的症状是上游返回一个看不懂的错误码。
func ValidateRequest(r Request) error {
	return validateBasic(r)
}

// ValidateRequestWithLangs 在 ValidateRequest 基础上追加规范语言码校验。
//
// 拆成两个函数是因为部分适配器（如 localcli）把语言名直接写进 Prompt，
// 不需要严格校验码表；而 REST 类适配器必须校验，否则会把非法码发给上游。
func ValidateRequestWithLangs(r Request) error {
	if err := validateBasic(r); err != nil {
		return err
	}
	if !isValidLang(r.TargetLang) {
		return Newf(KindConfig, "未知的目标语言码：%s", r.TargetLang)
	}
	if r.SourceLang != "" && !isValidLang(r.SourceLang) {
		return Newf(KindConfig, "未知的源语言码：%s", r.SourceLang)
	}
	return nil
}

func validateBasic(r Request) error {
	if strings.TrimSpace(r.Source) == "" {
		return New(KindConfig, "待翻译文本不能为空")
	}
	if strings.TrimSpace(r.TargetLang) == "" {
		return New(KindConfig, "目标语言不能为空")
	}
	if r.TargetLang == "auto" {
		return New(KindConfig, "目标语言不能是「自动检测」")
	}
	return nil
}

// isValidLang 判断是否为合法规范语言码。
//
// 用字符串集合而非调用 lang 包：engine 包被 lang 包间接依赖时会产生循环导入
// （lang 不依赖 engine，但 engine/openai 依赖 lang；若 engine 也依赖 lang，
// 将来 lang 想引用 engine 的类型就会成环）。这里用本地白名单，
// 与 lang.Canonical 保持同步由 lang 包的测试保证。
func isValidLang(code string) bool {
	switch code {
	case "auto", "zh", "zh-TW", "en", "ja", "ko", "fr", "de", "es", "ru",
		"pt", "it", "ar", "th", "vi", "id", "ms", "tr", "pl", "nl",
		"sv", "da", "fi", "no", "cs", "el", "he", "hi", "uk", "ro", "hu":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// 网络错误分类
// ---------------------------------------------------------------------------

// ClassifyTransportErr 把网络层错误归类，并给出可操作的中文提示。
//
// 区分几类常见情况，因为它们的用户处理建议完全不同：
//   - DNS 解析失败 → 端点地址写错了
//   - 连接被拒     → 服务没启动或端口错了
//   - TLS 失败     → 证书问题（提示可以开启跳过校验）
//   - 超时         → 网络慢或服务端慢
//
// 不区分的话用户只会看到 "Post https://...: dial tcp: lookup ...: no such host"，
// 完全不知道该改配置还是该重试。
func ClassifyTransportErr(err error, endpoint string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return Wrap(KindCancelled, "翻译已取消", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Wrap(KindNetwork, "请求超时", err).WithDetail("端点：%s", endpoint)
	}

	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "server misbehaving"):
		return Wrap(KindNetwork, "域名解析失败：请检查接口地址是否正确", err).
			WithDetail("端点：%s", endpoint)
	case strings.Contains(msg, "connection refused"):
		return Wrap(KindNetwork, "连接被拒绝：服务可能未启动，或端口不正确", err).
			WithDetail("端点：%s", endpoint)
	case strings.Contains(msg, "x509"), strings.Contains(msg, "certificate"),
		strings.Contains(msg, "tls:"):
		return Wrap(KindConfig,
			"TLS 证书校验失败：证书可能已过期或为自签名。内网端点可开启「跳过 TLS 证书校验」", err).
			WithDetail("端点：%s", endpoint)
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline"):
		return Wrap(KindNetwork, "请求超时", err).WithDetail("端点：%s", endpoint)
	case strings.Contains(msg, "proxy"):
		return Wrap(KindNetwork, "代理连接失败：请检查系统代理设置", err)
	case strings.Contains(msg, "connection reset"), strings.Contains(msg, "broken pipe"),
		strings.Contains(msg, "eof"):
		return Wrap(KindNetwork, "连接被中断：请重试", err).WithDetail("端点：%s", endpoint)
	}
	return Wrap(KindNetwork, "网络请求失败", err).WithDetail("端点：%s", endpoint)
}

// ---------------------------------------------------------------------------
// 响应体解析辅助
// ---------------------------------------------------------------------------

// ExtractJSONMessage 从错误响应体里提取人类可读的消息。
//
// 兼容三种常见形状：
//   - {"error":{"message":"..."}}   OpenAI / DeepL 风格
//   - {"message":"..."}             简化风格
//   - {"detail":"..."}              FastAPI 风格
//
// 提取失败返回空串（调用方退回用状态码说明）。
// 【绝不】返回整个 body —— 那可能是几 KB 的 HTML 错误页。
func ExtractJSONMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	trimmed := strings.TrimSpace(string(body))
	// HTML 错误页（网关返回）直接放弃提取
	if strings.HasPrefix(trimmed, "<") {
		return ""
	}
	var probe struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	if json.Unmarshal([]byte(trimmed), &probe) != nil {
		return ""
	}
	for _, candidate := range []string{probe.Error.Message, probe.Message, probe.Detail} {
		if s := strings.TrimSpace(candidate); s != "" {
			return TruncateRunes(s, 200)
		}
	}
	return ""
}

// Snippet 返回响应体片段用于诊断，截断到 300 字符。
func Snippet(body []byte) string {
	return TruncateRunes(strings.TrimSpace(string(body)), 300)
}

// TruncateRunes 按【字符】截断（不是字节），避免把多字节字符切成两半。
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "…"
}

// ---------------------------------------------------------------------------
// 译文清洗
// ---------------------------------------------------------------------------

// CleanTranslation 清洗模型输出，剥离常见的包裹与前后缀。
//
// 只有 LLM 类引擎需要（openai / localcli）：传统机翻 API 返回的就是纯译文。
//
// 即使系统提示词明确要求「只输出译文」，模型仍经常：
//   - 把译文包在引号里："你好"
//   - 加前缀：译文：你好 / Translation: Hello
//   - 用代码块包裹：``` 你好 ```
//
// 这些都会让译文不可用，必须后处理。
func CleanTranslation(s string) string {
	out := strings.TrimSpace(s)
	if out == "" {
		return ""
	}
	// 剥离整体包裹的代码块围栏
	if strings.HasPrefix(out, "```") {
		if i := strings.IndexByte(out, '\n'); i >= 0 {
			out = out[i+1:]
		} else {
			out = strings.TrimPrefix(out, "```")
		}
		out = strings.TrimSpace(out)
		out = strings.TrimSuffix(out, "```")
		out = strings.TrimSpace(out)
	}
	// 剥离常见前缀（中英文）
	for _, prefix := range []string{
		"译文：", "译文:", "翻译：", "翻译:", "结果：", "结果:",
		"Translation:", "translation:", "Translated text:",
		"Here is the translation:", "Here is the translation",
	} {
		if strings.HasPrefix(out, prefix) {
			out = strings.TrimSpace(out[len(prefix):])
			break
		}
	}
	out = StripWrappingQuotes(out)
	return strings.TrimSpace(out)
}

// StripWrappingQuotes 剥离首尾成对的引号。
//
// 必须成对才剥：如果只剥开头的引号，会把「"Hello" 是一个词」
// 变成 `Hello" 是一个词`，破坏正文。
func StripWrappingQuotes(s string) string {
	r := []rune(s)
	if len(r) < 2 {
		return s
	}
	first, last := r[0], r[len(r)-1]
	pairs := [][2]rune{{'"', '"'}, {'“', '”'}, {'「', '」'}, {'『', '』'}, {'\'', '\''}}
	for _, p := range pairs {
		if first == p[0] && last == p[1] {
			inner := strings.TrimSpace(string(r[1 : len(r)-1]))
			// 只在剥离后仍非空、且内部不含未配对引号时才接受，
			// 避免把「"A" 和 "B"」错误地剥成 `A" 和 "B`
			if inner != "" && !strings.ContainsAny(inner, `"'“”`) {
				return inner
			}
			return s
		}
	}
	return s
}

// ---------------------------------------------------------------------------
// 配置读取
// ---------------------------------------------------------------------------

// ConfigFloat 从 Config 里安全地取浮点值并夹到 [lo, hi]。
func ConfigFloat(cfg Config, key string, def, lo, hi float64) float64 {
	if cfg == nil {
		return def
	}
	v, ok := cfg[key]
	if !ok || v == nil {
		return def
	}
	var n float64
	switch t := v.(type) {
	case float64:
		n = t
	case int:
		n = float64(t)
	case int64:
		n = float64(t)
	case string:
		// 前端 number 输入框在某些情况下会传字符串
		var f float64
		if _, err := json.Number(t).Float64(); err == nil {
			f, _ = json.Number(t).Float64()
		} else {
			return def
		}
		n = f
	default:
		return def
	}
	if n != n || n > 1e18 || n < -1e18 { // NaN / Inf
		return def
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// DisplayName 返回引擎的展示名（用于历史记录）。
func DisplayName(optsName, fallback string) string {
	if strings.TrimSpace(optsName) != "" {
		return optsName
	}
	return fallback
}
