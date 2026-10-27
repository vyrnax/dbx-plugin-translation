// Package aliyun 实现阿里云机器翻译（TranslateGeneral）引擎。
//
// # 签名算法：ACS3-HMAC-SHA256
//
// 阿里云的签名比百度/有道复杂得多，分四步：
//
//  1. 构造规范请求（CanonicalRequest）：
//     METHOD\n
//     CanonicalURI\n
//     CanonicalQueryString\n
//     CanonicalHeaders\n
//     SignedHeaders\n
//     Hex(SHA256(payload))
//
//  2. 构造待签名串：ACS3-HMAC-SHA256 换行 Hex(SHA256(CanonicalRequest))
//
//  3. Signature = Hex(HMAC-SHA256(secretKey, StringToSign))
//
//  4. Authorization 头：
//     ACS3-HMAC-SHA256 Credential=<AK>,SignedHeaders=<...>,Signature=<...>
//
// 每一处的规范化规则都不能错：查询参数按名排序且用 RFC3986 编码
// （空格是 %20 不是加号），头名全小写且排序，SignedHeaders 用分号连接。
// 任何一处写错的症状都是 "SignatureDoesNotMatch"，不会告诉你错在哪。
package aliyun

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/vyrnax/translation/engine"
	"github.com/vyrnax/translation/lang"
)

// Kind 是本引擎的类型标识。
const Kind = "aliyun"

// maxCharsPerRequest 是单次请求的字符上限（阿里云限制 5000 字符）。
const maxCharsPerRequest = 4500

const (
	defaultRegion = "cn-hangzhou"
	apiVersion    = "2018-10-12"
)

const testProbeText = "hello"

func init() {
	engine.Register(Kind, build, schema())
}

type engineImpl struct {
	endpoint        string
	accessKeyID     string
	accessKeySecret string
	region          string
	scene           string
	client          *http.Client
	name            string
}

func build(cfg engine.Config, sec engine.Secrets, opts engine.Options) (engine.Engine, error) {
	akID := strings.TrimSpace(engine.ConfigString(cfg, "accessKeyId", ""))
	if akID == "" {
		return nil, engine.New(engine.KindConfig, "阿里云 AccessKey ID 不能为空")
	}
	akSecret := engine.SecretValue(sec, "accessKeySecret")
	if akSecret == "" {
		return nil, engine.New(engine.KindConfig, "阿里云 AccessKey Secret 不能为空")
	}

	region := strings.TrimSpace(engine.ConfigString(cfg, "region", defaultRegion))
	if region == "" {
		region = defaultRegion
	}

	endpoint := strings.TrimSpace(engine.ConfigString(cfg, "endpoint", ""))
	if endpoint == "" {
		endpoint = "https://mt." + region + ".aliyuncs.com"
	} else {
		normalized, _, err := engine.ValidateEndpoint(endpoint)
		if err != nil {
			return nil, err
		}
		endpoint = normalized
	}

	scene := engine.ConfigString(cfg, "scene", "general")
	switch scene {
	case "general", "title", "description", "communication", "medical", "social":
	default:
		scene = "general"
	}

	return &engineImpl{
		endpoint:        strings.TrimRight(endpoint, "/"),
		accessKeyID:     akID,
		accessKeySecret: akSecret,
		region:          region,
		scene:           scene,
		client: engine.NewHTTPClient(
			engine.ConfigInt(cfg, "timeoutSecs", engine.DefaultTimeoutSecs, 5, 600),
			engine.ConfigBool(cfg, "skipTlsVerify", false)),
		name: opts.Name,
	}, nil
}

func (e *engineImpl) Kind() string             { return Kind }
func (e *engineImpl) SupportsStreaming() bool  { return false }
func (e *engineImpl) MaxChars() int            { return maxCharsPerRequest }
func (e *engineImpl) SupportedLangs() []string { return lang.SupportedLangs(Kind) }

type translateResponse struct {
	Code      string `json:"Code"`
	Message   string `json:"Message"`
	RequestID string `json:"RequestId"`
	Data      struct {
		Translated string `json:"Translated"`
	} `json:"Data"`
}

func (e *engineImpl) Translate(ctx context.Context, r engine.Request) (engine.Result, error) {
	if err := engine.ValidateRequest(r); err != nil {
		return engine.Result{}, err
	}
	if n := len([]rune(r.Source)); n > maxCharsPerRequest {
		return engine.Result{}, engine.Newf(engine.KindConfig,
			"文本过长（%d 字符）：阿里云翻译单次上限为 %d 字符。请分段翻译", n, maxCharsPerRequest)
	}
	start := time.Now()

	src := "auto"
	if r.SourceLang != "" && r.SourceLang != lang.Auto {
		code, ok := lang.MapSource(Kind, r.SourceLang)
		if !ok {
			return engine.Result{}, engine.Newf(engine.KindUnsupported,
				"阿里云翻译不支持源语言 %s", lang.ZhName(r.SourceLang))
		}
		src = code
	}
	dst, ok := lang.MapTarget(Kind, r.TargetLang)
	if !ok {
		return engine.Result{}, engine.Newf(engine.KindUnsupported,
			"阿里云翻译不支持目标语言 %s", lang.ZhName(r.TargetLang))
	}

	// 阿里云 RPC 风格：业务参数全部放查询串，payload 为空
	params := map[string]string{
		"Action":         "TranslateGeneral",
		"Version":        apiVersion,
		"Format":         "JSON",
		"FormatType":     "text",
		"SourceLanguage": src,
		"TargetLanguage": dst,
		"SourceText":     r.Source,
		"Scene":          e.scene,
	}
	req, err := e.buildRequest(ctx, params, []byte{})
	if err != nil {
		return engine.Result{}, err
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return engine.Result{}, engine.ClassifyTransportErr(err, e.endpoint)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, engine.MaxResponseBody()))
	if err != nil {
		return engine.Result{}, engine.Wrap(engine.KindNetwork, "读取响应失败", err)
	}

	var parsed translateResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return engine.Result{}, engine.Wrap(engine.KindProtocol, "响应不是合法的 JSON", err).
			WithDetail("片段：%s", engine.Snippet(raw))
	}

	// 阿里云用 HTTP 200 + Code 字段表示业务错误（Code 为 "200" 表示成功）
	if parsed.Code != "" && parsed.Code != "200" {
		return engine.Result{}, classifyErrorCode(parsed.Code, parsed.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return engine.Result{}, classifyHTTPStatus(resp.StatusCode, raw)
	}

	target := parsed.Data.Translated
	if strings.TrimSpace(target) == "" {
		return engine.Result{}, engine.New(engine.KindProtocol,
			"阿里云翻译返回了空译文").WithDetail("片段：%s", engine.Snippet(raw))
	}

	return engine.Result{
		Target:    target,
		ElapsedMs: engine.ElapsedMs(start),
		Chunks:    1,
		Engine:    engine.DisplayName(e.name, "阿里云翻译"),
		Model:     "aliyun-mt",
	}, nil
}

// buildRequest 构造带 ACS3-HMAC-SHA256 签名的请求。
func (e *engineImpl) buildRequest(ctx context.Context, params map[string]string, payload []byte) (*http.Request, error) {
	u, err := url.Parse(e.endpoint)
	if err != nil {
		return nil, engine.Wrap(engine.KindConfig, "接口地址非法", err)
	}
	host := u.Host
	path := u.Path
	if path == "" {
		path = "/"
	}

	now := time.Now().UTC()
	// x-acs-date 必须是 ISO8601 UTC，精确到秒；与实际时间偏差超过 15 分钟会签名失败
	acsDate := now.Format("2006-01-02T15:04:05Z")
	nonce := randomHex(16)
	payloadHash := sha256Hex(payload)
	contentType := "application/x-www-form-urlencoded"

	headers := map[string]string{
		"content-type":          contentType,
		"host":                  host,
		"x-acs-action":          params["Action"],
		"x-acs-content-sha256":  payloadHash,
		"x-acs-date":            acsDate,
		"x-acs-signature-nonce": nonce,
		"x-acs-version":         params["Version"],
	}

	canonicalRequest := buildCanonicalRequest(http.MethodPost, path, params, headers, payloadHash)
	stringToSign := "ACS3-HMAC-SHA256\n" + sha256Hex([]byte(canonicalRequest))
	signature := hmacSHA256Hex(e.accessKeySecret, stringToSign)

	signedHeaders := sortedHeaderNames(headers)
	auth := "ACS3-HMAC-SHA256 Credential=" + e.accessKeyID +
		",SignedHeaders=" + strings.Join(signedHeaders, ";") +
		",Signature=" + signature

	reqURL := e.endpoint + "?" + canonicalQueryString(params)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(""))
	if err != nil {
		return nil, engine.Wrap(engine.KindConfig, "构造请求失败", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("x-acs-action", params["Action"])
	req.Header.Set("x-acs-content-sha256", payloadHash)
	req.Header.Set("x-acs-date", acsDate)
	req.Header.Set("x-acs-signature-nonce", nonce)
	req.Header.Set("x-acs-version", params["Version"])
	req.Header.Set("Authorization", auth)
	req.Host = host
	return req, nil
}

// buildCanonicalRequest 按 ACS3 规范拼出规范请求。
func buildCanonicalRequest(method, path string, params, headers map[string]string, payloadHash string) string {
	signed := sortedHeaderNames(headers)
	var canonicalHeaders strings.Builder
	for _, name := range signed {
		canonicalHeaders.WriteString(name)
		canonicalHeaders.WriteString(":")
		canonicalHeaders.WriteString(strings.TrimSpace(headers[name]))
		canonicalHeaders.WriteString("\n")
	}
	return method + "\n" +
		path + "\n" +
		canonicalQueryString(params) + "\n" +
		canonicalHeaders.String() + "\n" +
		strings.Join(signed, ";") + "\n" +
		payloadHash
}

// canonicalQueryString 按参数名字典序排序，键值均做 RFC3986 编码后拼接。
func canonicalQueryString(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, percentEncode(k)+"="+percentEncode(params[k]))
	}
	return strings.Join(parts, "&")
}

// percentEncode 实现 RFC3986 编码。
//
// 不能用 url.QueryEscape：它把空格编码成加号，而 ACS3 要求 %20；
// 且它对 ~ 的处理也不一致。这两点都会导致签名不匹配。
func percentEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteString("%")
		b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
	}
	return b.String()
}

func sortedHeaderNames(headers map[string]string) []string {
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, strings.ToLower(k))
	}
	sort.Strings(names)
	return names
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256Hex(key, data string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(data))
	return hex.EncodeToString(mac.Sum(nil))
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败时退化为时间戳，保证请求不被阻塞
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b)
}

func (e *engineImpl) Stream(ctx context.Context, r engine.Request, emit func(engine.Chunk) error) (engine.Result, error) {
	res, err := e.Translate(ctx, r)
	if err != nil {
		return engine.Result{}, err
	}
	if emit != nil {
		if err := emit(engine.Chunk{Text: res.Target, Index: 0, Total: 1}); err != nil {
			return engine.Result{}, engine.Wrap(engine.KindCancelled, "翻译已取消", err)
		}
	}
	return res, nil
}

func (e *engineImpl) Test(ctx context.Context) error {
	_, err := e.Translate(ctx, engine.Request{
		Source: testProbeText, SourceLang: "en", TargetLang: "zh",
	})
	return err
}

// classifyErrorCode 把阿里云的业务错误码映射为统一错误类型。
func classifyErrorCode(code, msg string) error {
	detail := msg
	if detail == "" {
		detail = "（阿里云未返回错误描述）"
	}
	low := strings.ToLower(code + " " + detail)

	switch {
	case strings.Contains(low, "signaturedoesnotmatch"), strings.Contains(low, "incompletesignature"):
		return engine.New(engine.KindAuth,
			"签名不匹配（"+code+"）：请检查 AccessKey Secret 是否正确，以及本机时间是否准确（偏差超过 15 分钟会签名失败）").
			WithDetail("阿里云说明：%s", detail)
	case strings.Contains(low, "invalidaccesskeyid"), strings.Contains(low, "accesskeyid"):
		return engine.New(engine.KindAuth,
			"AccessKey ID 无效（"+code+"）：请检查是否填写正确、是否已被禁用").
			WithDetail("阿里云说明：%s", detail)
	case strings.Contains(low, "throttling"), strings.Contains(low, "flowcontrol"):
		return engine.New(engine.KindRateLimit,
			"请求被限流（"+code+"）：请降低并发数或增大分段间隔").
			WithDetail("阿里云说明：%s", detail)
	case strings.Contains(low, "forbidden"), strings.Contains(low, "nopermission"),
		strings.Contains(low, "unauthorized"):
		return engine.New(engine.KindAuth,
			"无权限（"+code+"）：请在阿里云 RAM 控制台为该 AccessKey 授予「机器翻译」权限").
			WithDetail("阿里云说明：%s", detail)
	case strings.Contains(low, "invalidparameter"), strings.Contains(low, "missingparameter"):
		return engine.New(engine.KindConfig,
			"参数非法（"+code+"）：请检查语言与文本设置").
			WithDetail("阿里云说明：%s", detail)
	case strings.Contains(low, "unsupportedlanguage"), strings.Contains(low, "languagenotsupport"):
		return engine.New(engine.KindUnsupported,
			"不支持的语言（"+code+"）：请换一个语言").
			WithDetail("阿里云说明：%s", detail)
	case strings.Contains(low, "serviceunavailable"), strings.Contains(low, "internalerror"):
		return engine.New(engine.KindUpstream,
			"阿里云服务暂时不可用（"+code+"）：请稍后重试").
			WithDetail("阿里云说明：%s", detail)
	}
	return engine.Newf(engine.KindUpstream, "阿里云翻译返回错误 %s：%s", code, detail)
}

func classifyHTTPStatus(status int, body []byte) error {
	if kind, isErr := engine.ClassifyHTTPStatus(status); isErr {
		e := engine.New(kind, engine.StatusMessage(status))
		if msg := engine.ExtractJSONMessage(body); msg != "" {
			e.Message += "：" + msg
		}
		return e.WithDetail("片段：%s", engine.Snippet(body))
	}
	return engine.Newf(engine.KindUpstream, "阿里云翻译返回意外状态码 %d", status).
		WithDetail("片段：%s", engine.Snippet(body))
}
