// Package deepl 实现 DeepL 翻译引擎。
//
// DeepL 是纯 REST 接口，没有流式能力，因此 SupportsStreaming 返回 false，
// 调用方会自动走「单 chunk 降级」路径 —— 前端代码路径与真流式完全一致。
//
// # 端点与 Key 必须匹配
//
// DeepL 有两套官方端点，用【不同的 Key】，混用会返回 403 且错误信息
// 不会提示原因（只写 "Wrong endpoint"）：
//
//	api-free.deepl.com  免费档，Key 以 ":fx" 结尾
//	api.deepl.com       付费档
//
// 因此 build() 会提前校验 Key 与端点的匹配性，省掉一次必然失败的往返。
//
// # 认证方式
//
//	Authorization: DeepL-Auth-Key <key>
//
// 不是 Bearer。写错会返回 403 而非 401，很容易被误判为 Key 无效。
package deepl

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vyrnax/translation/engine"
	"github.com/vyrnax/translation/lang"
)

// Kind 是本引擎的类型标识。
const Kind = "deepl"

// maxCharsPerRequest 是单次请求的字符上限。
//
// DeepL 官方限制是 128 KiB（按 UTF-8 字节计）。换算成字符时按最保守的
// 假设（1 字符 = 3 字节，覆盖中文）得 128*1024/3 ≈ 43000，取 40000 留余量。
const maxCharsPerRequest = 40000

const (
	endpointFree = "https://api-free.deepl.com/v2/translate"
	endpointPro  = "https://api.deepl.com/v2/translate"
)

// testProbeText 是连通性探测文本（最短常见词，省配额）。
const testProbeText = "hello"

func init() {
	engine.Register(Kind, build, schema())
}

type engineImpl struct {
	endpoint   string
	apiKey     string
	formality  string
	glossaryID string
	client     *http.Client
	name       string
}

func build(cfg engine.Config, sec engine.Secrets, opts engine.Options) (engine.Engine, error) {
	apiKey := engine.SecretValue(sec, "apiKey")
	if apiKey == "" {
		return nil, engine.New(engine.KindConfig, "DeepL API Key 不能为空")
	}

	// isOfficialEndpoint 标记端点是否为 DeepL 官方域名。
	//
	// 只有官方端点才做「Key 档位与端点匹配」检查 —— 用户填自定义端点
	// （企业代理、自建网关、测试用的 httptest 地址）时，我们无法判断
	// 它背后转发到哪个档位，此时强行检查会误拒合法配置。
	isOfficialEndpoint := false

	endpoint := strings.TrimSpace(engine.ConfigString(cfg, "endpoint", ""))
	if endpoint == "" {
		if engine.ConfigString(cfg, "serverKind", "free") == "pro" {
			endpoint = endpointPro
		} else {
			endpoint = endpointFree
		}
		isOfficialEndpoint = true
	} else {
		normalized, _, err := engine.ValidateEndpoint(endpoint)
		if err != nil {
			return nil, err
		}
		endpoint = normalized
		// 用户只填到域名时补全路径
		if !strings.Contains(endpoint, "/translate") {
			endpoint = strings.TrimRight(endpoint, "/") + "/v2/translate"
		}
		// 判断是否为官方域名（用户也可能显式填官方地址）
		isOfficialEndpoint = strings.Contains(endpoint, "api-free.deepl.com") ||
			strings.Contains(endpoint, "api.deepl.com")
	}

	// Key 与端点匹配性检查：混用必 403，且错误信息不说明原因。
	// 【仅对官方端点生效】，理由见上方 isOfficialEndpoint 的注释。
	if isOfficialEndpoint {
		isFreeKey := strings.HasSuffix(apiKey, ":fx")
		isFreeEndpoint := strings.Contains(endpoint, "api-free.deepl.com")
		if isFreeKey && !isFreeEndpoint {
			return nil, engine.New(engine.KindConfig,
				"这是 DeepL 免费档的 Key（以 :fx 结尾），但接口地址不是免费端点。"+
					"请把「服务档位」改为「免费档」，或换用付费档的 Key")
		}
		if !isFreeKey && isFreeEndpoint {
			return nil, engine.New(engine.KindConfig,
				"这是 DeepL 付费档的 Key，但接口地址是免费端点 api-free.deepl.com。"+
					"请把「服务档位」改为「付费档」")
		}
	}

	formality := engine.ConfigString(cfg, "formality", "default")
	switch formality {
	case "default", "more", "less", "prefer_more", "prefer_less":
	default:
		formality = "default"
	}

	return &engineImpl{
		endpoint:   endpoint,
		apiKey:     apiKey,
		formality:  formality,
		glossaryID: strings.TrimSpace(engine.ConfigString(cfg, "glossaryId", "")),
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
	Translations []struct {
		DetectedSourceLanguage string `json:"detected_source_language"`
		Text                   string `json:"text"`
	} `json:"translations"`
	Message string `json:"message"`
}

func (e *engineImpl) Translate(ctx context.Context, r engine.Request) (engine.Result, error) {
	if err := engine.ValidateRequest(r); err != nil {
		return engine.Result{}, err
	}
	start := time.Now()

	// DeepL 用表单编码，不是 JSON
	form := url.Values{}
	form.Set("text", r.Source)

	// 源语言：DeepL 用「省略 source_lang」表示自动检测。
	// 传空串会被判为非法参数（400），因此 auto 时整个字段都不发。
	if r.SourceLang != "" && r.SourceLang != lang.Auto {
		src, ok := lang.MapSource(Kind, r.SourceLang)
		if !ok {
			return engine.Result{}, engine.Newf(engine.KindUnsupported,
				"DeepL 不支持源语言 %s", lang.ZhName(r.SourceLang))
		}
		if src != "" {
			form.Set("source_lang", src)
		}
	}
	dst, ok := lang.MapTarget(Kind, r.TargetLang)
	if !ok {
		return engine.Result{}, engine.Newf(engine.KindUnsupported,
			"DeepL 不支持目标语言 %s", lang.ZhName(r.TargetLang))
	}
	form.Set("target_lang", dst)

	// formality 只对部分目标语言有效，对其它语言传了会被忽略而非报错，
	// 所以无条件带上（非 default 时）是安全的。
	if e.formality != "" && e.formality != "default" {
		form.Set("formality", e.formality)
	}
	if e.glossaryID != "" {
		// 使用术语表时 DeepL 强制要求显式源语言，否则 400
		if form.Get("source_lang") == "" {
			return engine.Result{}, engine.New(engine.KindConfig,
				"使用 DeepL 术语表时必须显式指定源语言（不支持自动检测）")
		}
		form.Set("glossary_id", e.glossaryID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return engine.Result{}, engine.Wrap(engine.KindConfig, "构造请求失败", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "DeepL-Auth-Key "+e.apiKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return engine.Result{}, engine.ClassifyTransportErr(err, e.endpoint)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, engine.MaxResponseBody()))
	if err != nil {
		return engine.Result{}, engine.Wrap(engine.KindNetwork, "读取响应失败", err)
	}
	if resp.StatusCode != http.StatusOK {
		return engine.Result{}, classifyAPIError(resp.StatusCode, raw)
	}

	var parsed translateResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return engine.Result{}, engine.Wrap(engine.KindProtocol, "响应不是合法的 JSON", err).
			WithDetail("片段：%s", engine.Snippet(raw))
	}
	if len(parsed.Translations) == 0 {
		msg := parsed.Message
		if msg == "" {
			msg = "响应中没有 translations"
		}
		return engine.Result{}, engine.New(engine.KindProtocol, "DeepL 返回了空结果："+msg).
			WithDetail("片段：%s", engine.Snippet(raw))
	}

	target := parsed.Translations[0].Text
	if strings.TrimSpace(target) == "" {
		return engine.Result{}, engine.New(engine.KindProtocol, "DeepL 返回了空译文")
	}

	detected := ""
	if d := parsed.Translations[0].DetectedSourceLanguage; d != "" {
		detected = toCanonical(d)
	}

	return engine.Result{
		Target:       target,
		DetectedLang: detected,
		ElapsedMs:    time.Since(start).Milliseconds(),
		Chunks:       1,
		Engine:       engine.DisplayName(e.name, "DeepL"),
		Model:        "deepl",
	}, nil
}

// Stream 对不支持流式的引擎做单 chunk 降级。
//
// main.go 会先查 SupportsStreaming() 再决定走哪条路径，但接口要求实现这个方法。
// 直接调用它也必须能正常工作 —— 返回「不支持」会让将来直接调 Stream 的代码
// 拿到意外失败。
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

// toCanonical 把 DeepL 的私有码转回规范码。
// 前端语言下拉用的是规范码，对不上就无法高亮。
//
// 【必须同时匹配源形态与目标形态】：DeepL 的 detected_source_language
// 返回的是【源语言形态】的码（英语是 EN），而 lang.Map 返回的是
// 目标形态（EN-US）。只比对目标形态的话，英语永远匹配不上，
// detectedLang 会一直是空 —— 这是引入 sourceOverrides 后新产生的不一致。
func toCanonical(deeplCode string) string {
	upper := strings.ToUpper(strings.TrimSpace(deeplCode))
	if upper == "" {
		return ""
	}
	for _, l := range lang.All() {
		// 先比源形态（detected_source_language 用的就是这个）
		if code, ok := lang.MapSource(Kind, l.Code); ok && strings.ToUpper(code) == upper {
			return l.Code
		}
		// 再比目标形态（EN-US / PT-PT 等带地区后缀的）
		if code, ok := lang.MapTarget(Kind, l.Code); ok && strings.ToUpper(code) == upper {
			return l.Code
		}
	}
	return ""
}

// classifyAPIError 把 DeepL 的状态码归类。
//
// DeepL 的错误码语义与通用 REST 不同，必须单独映射：
//
//	403 既可能是 Key 无效，也可能是【用错端点】
//	456 是配额耗尽（DeepL 专有，不是标准 429）
//	429 是请求过于频繁（与 456 不同）
func classifyAPIError(status int, body []byte) error {
	detail := engine.ExtractJSONMessage(body)

	switch status {
	case http.StatusBadRequest:
		base := "请求被拒绝（400）"
		if detail != "" {
			base += "：" + detail
		} else {
			base += "：可能是语言不支持，或使用术语表时未指定源语言"
		}
		return engine.New(engine.KindConfig, base).WithDetail("片段：%s", engine.Snippet(body))
	case http.StatusForbidden:
		low := strings.ToLower(detail)
		// DeepL 有时用 403 表示配额耗尽，这种情况归为限流而非鉴权
		if strings.Contains(low, "quota") || strings.Contains(low, "limit") {
			return engine.New(engine.KindRateLimit, "DeepL 配额已用尽（403）").
				WithDetail("片段：%s", engine.Snippet(body))
		}
		base := "鉴权失败（403）：API Key 无效，或用错了端点（免费档 Key 以 :fx 结尾，必须配 api-free.deepl.com）"
		if detail != "" {
			base += "；DeepL 说明：" + detail
		}
		return engine.New(engine.KindAuth, base).WithDetail("片段：%s", engine.Snippet(body))
	case http.StatusNotFound:
		return engine.New(engine.KindConfig,
			"接口不存在（404）：请检查接口地址是否为 .../v2/translate").
			WithDetail("片段：%s", engine.Snippet(body))
	case http.StatusRequestEntityTooLarge:
		return engine.New(engine.KindConfig,
			"文本过长（413）：DeepL 单次请求上限为 128 KiB，请缩短文本")
	case http.StatusTooManyRequests:
		return engine.New(engine.KindRateLimit,
			"请求过于频繁（429）：请稍后再试，或降低并发数与缩短分段间隔")
	case 456:
		// DeepL 专有状态码：配额耗尽。重试无用，必须让用户知道是配额问题
		return engine.New(engine.KindRateLimit,
			"DeepL 配额已用尽（456）：本计费周期的字符额度已用完，请升级套餐或等待下个周期")
	case http.StatusServiceUnavailable:
		return engine.New(engine.KindUpstream, "DeepL 服务过载（503）：请稍后重试")
	}

	if kind, isErr := engine.ClassifyHTTPStatus(status); isErr {
		e := engine.New(kind, engine.StatusMessage(status))
		if detail != "" {
			e.Message += "：" + detail
		}
		return e.WithDetail("片段：%s", engine.Snippet(body))
	}
	return engine.Newf(engine.KindUpstream, "DeepL 返回意外状态码 %d", status).
		WithDetail("片段：%s", engine.Snippet(body))
}
