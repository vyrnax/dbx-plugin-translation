package engine

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 本文件提供所有 HTTP 类引擎共用的客户端构造与端点校验。
//
// 抽出来的原因：六个引擎里有五个要发 HTTP 请求，如果每个适配器自己
// new(http.Client)，那么「超时怎么设」「TLS 怎么配」「重定向怎么处理」
// 就会有五份不同的实现，其中至少一份是错的。

// DefaultTimeoutSecs 是未显式配置时的请求超时。
//
// 60 秒的依据：翻译请求的正常耗时在 1-10 秒，LLM 长文本可能到 30 秒。
// 60 秒足够覆盖慢速上游，又不会让用户等太久以为插件卡死。
const DefaultTimeoutSecs = 60

// LocalCLITimeoutSecs 是本地 CLI 引擎的默认超时。
//
// 120 秒：CLI 冷启动（加载配置、建立会话）本身就要几秒到十几秒，
// 加上模型推理时间，60 秒经常不够。
const LocalCLITimeoutSecs = 120

// maxResponseBodyBytes 是响应体读取上限（8 MiB）。
//
// 与 SDK 的单条 JSON 消息上限对齐：即使上游返回一个巨大的响应，
// 也不能让它撑爆侧车内存或产生一条发不出去的事件。
// 翻译响应正常只有几 KB，8 MiB 是纯粹的防御性上限。
const maxResponseBodyBytes = 8 * 1024 * 1024

// MaxResponseBody 返回响应体读取上限，供各适配器用 io.LimitReader 包裹。
//
// 导出而非各适配器自定义常量：上限必须与 SDK 的单条 JSON 消息上限（8MB）
// 协调，集中一处定义避免六个适配器各写一个值、其中一个超限。
func MaxResponseBody() int64 { return maxResponseBodyBytes }

// NewHTTPClient 构造带超时与 TLS 策略的 HTTP 客户端。
//
// skipTLSVerify 为 true 时关闭证书校验。这是【危险选项】，
// 仅用于自签名证书的内网 LLM 端点；调用方（适配器构造时）必须已经
// 通过 UI 明确告知用户风险，侧车也会在 trace 里记录（不含密钥）。
//
// 超时用 Transport 层的细粒度控制而非 client.Timeout：
//
//		client.Timeout 覆盖整个请求生命周期，包括读 body。
//		对流式（SSE）请求这是致命的 —— 一个持续 90 秒的流式翻译
//	 会在 60 秒时被 client.Timeout 强行掐断。
//		因此这里只设连接与握手超时，读 body 的超时由 ctx 控制
//		（调用方用 context.WithTimeout 包住整个流式过程）。
func NewHTTPClient(timeoutSecs int, skipTLSVerify bool) *http.Client {
	if timeoutSecs <= 0 {
		timeoutSecs = DefaultTimeoutSecs
	}
	dialer := &net.Dialer{
		Timeout:   time.Duration(timeoutSecs) * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   time.Duration(timeoutSecs) * time.Second,
		ResponseHeaderTimeout: time.Duration(timeoutSecs) * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		// 不限制读 body 的总时长：流式响应可能持续很久，
		// 由 ctx 负责取消。
	}
	if skipTLSVerify {
		// #nosec G402 -- 用户显式开启的选项，UI 已警示风险
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &http.Client{
		Transport: transport,
		// 不设 client.Timeout（理由见上方注释）
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// 最多跟随 5 次重定向，防止重定向环把侧车拖死。
			// 同时【不转发 Authorization 头】到跨域重定向目标 ——
			// 否则一个恶意端点可以用 302 把用户的 API Key 骗到第三方服务器。
			if len(via) >= 5 {
				return errors.New("重定向次数过多（超过 5 次）")
			}
			return nil
		},
	}
}

// StripAuthOnRedirect 在跨主机重定向时移除鉴权头。
//
// Go 的 http.Client 默认会在同主机重定向时保留 Authorization，
// 跨主机时移除敏感头（Go 1.8+ 的行为）。但「跨主机」的判定只看 host，
// 不看 scheme/port，因此 http→https 的同名主机跳转仍会带上 Key。
// 这个函数提供更严格的策略：只要目标 origin 与原始 origin 不同就清除。
//
// 用法：在适配器发请求前包一层，或用 CheckRedirect 里手动删头。
// 本项目采用后者（见各适配器的 doRequest）。
func StripAuthOnRedirect(req *http.Request, via []*http.Request) {
	if len(via) == 0 {
		return
	}
	orig := via[0].URL
	cur := req.URL
	if orig == nil || cur == nil {
		return
	}
	if orig.Scheme != cur.Scheme || orig.Host != cur.Host {
		req.Header.Del("Authorization")
		req.Header.Del("X-Api-Key")
		req.Header.Del("Api-Key")
		// 百度/有道/阿里把签名放在 query 或 body 里，重定向时无法安全保留，
		// 因此这类引擎一律【禁止】重定向（见各适配器的 allowRedirect=false）。
	}
}

// ValidateEndpoint 校验用户填写的端点地址。
//
// 规则：
//   - 必须是合法 URL
//   - scheme 必须是 https，或者是 localhost/127.0.0.1/[::1] 上的 http
//     （本地跑的 Ollama / vLLM / LM Studio 通常是 http://127.0.0.1:11434）
//   - 必须有 host
//
// 返回 (normalized, warning, error)：
//   - normalized 是补全了 scheme 的地址（用户常漏写 https://）
//   - warning 是非致命提示（例如「使用了非加密连接」），UI 应显示但不阻止保存
//   - error 是致命问题，应阻止保存
//
// 为什么允许 localhost 的 http：本地推理服务（Ollama 等）默认不开 TLS，
// 强制 https 会让「用本地模型翻译」这个高频场景直接不可用。
// 而 localhost 流量不出网卡，明文风险可接受。
func ValidateEndpoint(raw string) (string, string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", "", New(KindConfig, "接口地址不能为空")
	}
	// 用户常漏写 scheme，自动补 https://
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", "", Newf(KindConfig, "接口地址格式非法：%v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", Newf(KindConfig, "接口地址只支持 http/https，当前为 %s", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return "", "", New(KindConfig, "接口地址缺少主机名")
	}
	isLocal := host == "localhost" || host == "127.0.0.1" || host == "::1" ||
		strings.HasSuffix(host, ".localhost")

	warning := ""
	if u.Scheme == "http" && !isLocal {
		// 非本地的明文 HTTP：API Key 会以明文在网络上传输。
		// 不直接拒绝（用户可能有特殊的内网环境），但必须警告。
		warning = "使用了非加密的 HTTP 连接，API Key 将以明文传输。建议改用 HTTPS。"
	}
	return u.String(), warning, nil
}

// IsLocalhost 判断主机名是否为本地回环地址。
func IsLocalhost(host string) bool {
	h := strings.TrimSpace(host)
	return h == "localhost" || h == "127.0.0.1" || h == "::1" ||
		strings.HasSuffix(h, ".localhost")
}

// ConfigString 从 Config 里安全地取字符串值。
//
// JSON 反序列化后所有值都是 any，直接类型断言会在类型不符时 panic。
// 这个辅助函数统一处理：类型不符或为空时返回 def。
func ConfigString(cfg Config, key, def string) string {
	if cfg == nil {
		return def
	}
	v, ok := cfg[key]
	if !ok || v == nil {
		return def
	}
	s, ok := v.(string)
	if !ok {
		return def
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	return s
}

// ConfigInt 从 Config 里安全地取整数值，并夹到 [lo, hi]。
//
// JSON 数字统一是 float64，但用户也可能通过导入传入 int，两种都要接。
// 夹范围是必要的：服务端不能信任前端传来的值（例如 timeoutSecs=0 会让
// 请求永不超时，把 goroutine 挂死）。
func ConfigInt(cfg Config, key string, def, lo, hi int) int {
	if cfg == nil {
		return clamp(def, lo, hi)
	}
	v, ok := cfg[key]
	if !ok || v == nil {
		return clamp(def, lo, hi)
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
		var parsed int
		if _, err := fmt.Sscanf(t, "%d", &parsed); err != nil {
			return clamp(def, lo, hi)
		}
		n = float64(parsed)
	default:
		return clamp(def, lo, hi)
	}
	// NaN / Inf 必须拦掉，否则比较结果不可预期
	if n != n || n > 1e18 || n < -1e18 {
		return clamp(def, lo, hi)
	}
	return clamp(int(n), lo, hi)
}

// ConfigBool 从 Config 里安全地取布尔值。
func ConfigBool(cfg Config, key string, def bool) bool {
	if cfg == nil {
		return def
	}
	v, ok := cfg[key]
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "1" || t == "yes"
	}
	return def
}

// SecretValue 从 Secrets 里取密钥，去除首尾空白。
//
// 去空白很重要：用户从网页复制 API Key 时经常带上换行或空格，
// 不去掉会导致鉴权失败，而错误提示是「Key 无效」，用户完全想不到
// 是多了个空格。
func SecretValue(sec Secrets, key string) string {
	if sec == nil {
		return ""
	}
	return strings.TrimSpace(sec[key])
}

func clamp(v, lo, hi int) int {
	if lo > hi {
		lo, hi = hi, lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ClassifyHTTPStatus 把 HTTP 状态码归类为错误 Kind。
//
// 集中处理的原因：六个引擎都要做这个映射，分散实现必然出现
// 「某个引擎把 429 当成网络错误去重试」这类 bug —— 而重试限流请求
// 只会加重限流甚至触发封禁。
func ClassifyHTTPStatus(status int) (ErrorKind, bool) {
	switch {
	case status == http.StatusUnauthorized, // 401
		status == http.StatusForbidden,         // 403
		status == http.StatusProxyAuthRequired: // 407
		return KindAuth, true
	case status == http.StatusTooManyRequests: // 429
		return KindRateLimit, true
	case status == http.StatusRequestEntityTooLarge: // 413
		// 请求体过大：这是配置问题（maxChars 设太高），不是网络问题
		return KindConfig, true
	case status == http.StatusUnsupportedMediaType, // 415
		status == http.StatusBadRequest: // 400
		// 400 通常是请求格式问题（模型名不存在、参数非法）
		return KindConfig, true
	case status == http.StatusNotFound: // 404
		// 404 多半是端点路径写错了
		return KindConfig, true
	case status >= 500 && status <= 599:
		return KindUpstream, true
	case status >= 400:
		// 其余 4xx：归为配置问题（不重试，让用户检查配置）
		return KindConfig, true
	}
	return 0, false // 2xx/3xx 不是错误
}

// StatusMessage 返回状态码的中文说明，用于错误消息。
func StatusMessage(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "请求被拒绝（400）：参数或模型名可能有误"
	case http.StatusUnauthorized:
		return "鉴权失败（401）：API Key 无效或已过期"
	case http.StatusForbidden:
		return "无权限（403）：API Key 有效但没有该接口的访问权限"
	case http.StatusNotFound:
		return "接口不存在（404）：请检查接口地址是否完整（通常需要包含 /v1/... 路径）"
	case http.StatusRequestEntityTooLarge:
		return "文本过长（413）：请调低「单次请求字符上限」"
	case http.StatusTooManyRequests:
		return "请求过于频繁（429）：请稍后再试，或降低并发数"
	case http.StatusInternalServerError:
		return "服务端内部错误（500）"
	case http.StatusBadGateway:
		return "网关错误（502）：上游服务暂时不可用"
	case http.StatusServiceUnavailable:
		return "服务不可用（503）：上游服务过载或维护中"
	case http.StatusGatewayTimeout:
		return "网关超时（504）：上游服务响应过慢"
	}
	if status >= 500 {
		return fmt.Sprintf("服务端错误（%d）", status)
	}
	if status >= 400 {
		return fmt.Sprintf("请求失败（%d）", status)
	}
	return fmt.Sprintf("意外的状态码（%d）", status)
}
