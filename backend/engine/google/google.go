// Package google 实现 Google Cloud Translation API (v2) 引擎。
//
// # 认证方式与其它引擎不同
//
// Google v2 用【URL 查询参数】传 Key，而不是 Authorization 头：
//
//	POST https://translation.googleapis.com/language/translate/v2?key=<API_KEY>
//
// 这一点很容易踩坑：把 Key 放请求头会得到 403，而错误信息只说
// "The request is missing a valid API key"，不会告诉你是放错了位置。
//
// # 响应是 HTML 转义的
//
// Google 返回的 translatedText 做了 HTML 实体转义（' → &#39;、" → &quot;），
// 直接展示会给用户看到一堆 &#39;。必须反转义后再返回。
//
// # 不支持流式
//
// 与 DeepL 一样走单 chunk 降级路径。
package google

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gaogao/translation/engine"
	"github.com/gaogao/translation/lang"
)

// Kind 是本引擎的类型标识。
const Kind = "google"

// maxCharsPerRequest 是单次请求的字符上限。
//
// Google v2 单次请求最多 5000 字符（官方文档口径，按字符计），
// 保守取 4500 留余量。
const maxCharsPerRequest = 4500

const defaultEndpoint = "https://translation.googleapis.com/language/translate/v2"

// testProbeText 是连通性探测文本。
const testProbeText = "hello"

func init() {
	engine.Register(Kind, build, schema())
}

type engineImpl struct {
	endpoint string
	apiKey   string
	client   *http.Client
	name     string
}

func build(cfg engine.Config, sec engine.Secrets, opts engine.Options) (engine.Engine, error) {
	apiKey := engine.SecretValue(sec, "apiKey")
	if apiKey == "" {
		return nil, engine.New(engine.KindConfig, "Google API Key 不能为空")
	}

	endpoint := strings.TrimSpace(engine.ConfigString(cfg, "endpoint", ""))
	if endpoint == "" {
		endpoint = defaultEndpoint
	} else {
		normalized, _, err := engine.ValidateEndpoint(endpoint)
		if err != nil {
			return nil, err
		}
		endpoint = normalized
	}

	return &engineImpl{
		endpoint: endpoint,
		apiKey:   apiKey,
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
	Data struct {
		Translations []struct {
			TranslatedText         string `json:"translatedText"`
			DetectedSourceLanguage string `json:"detectedSourceLanguage"`
		} `json:"translations"`
	} `json:"data"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func (e *engineImpl) Translate(ctx context.Context, r engine.Request) (engine.Result, error) {
	if err := engine.ValidateRequest(r); err != nil {
		return engine.Result{}, err
	}
	start := time.Now()

	body := map[string]any{
		"q":      []string{r.Source},
		"format": "text",
	}
	// 源语言：Google 用「省略 source」表示自动检测
	if r.SourceLang != "" && r.SourceLang != lang.Auto {
		src, ok := lang.MapSource(Kind, r.SourceLang)
		if !ok {
			return engine.Result{}, engine.Newf(engine.KindUnsupported,
				"Google 翻译不支持源语言 %s", lang.ZhName(r.SourceLang))
		}
		if src != "" {
			body["source"] = src
		}
	}
	dst, ok := lang.MapTarget(Kind, r.TargetLang)
	if !ok {
		return engine.Result{}, engine.Newf(engine.KindUnsupported,
			"Google 翻译不支持目标语言 %s", lang.ZhName(r.TargetLang))
	}
	body["target"] = dst

	buf, err := json.Marshal(body)
	if err != nil {
		return engine.Result{}, engine.Wrap(engine.KindConfig, "构造请求失败", err)
	}

	// Key 走查询参数（Google v2 的约定），不是 Authorization 头
	url := e.endpoint
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	url += sep + "key=" + e.apiKey

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return engine.Result{}, engine.Wrap(engine.KindConfig, "构造请求失败", err)
	}
	req.Header.Set("Content-Type", "application/json")

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
	_ = json.Unmarshal(raw, &parsed)

	if resp.StatusCode != http.StatusOK {
		msg := ""
		if parsed.Error != nil {
			msg = parsed.Error.Message
		}
		return engine.Result{}, classifyAPIError(resp.StatusCode, msg, raw)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return engine.Result{}, engine.New(engine.KindUpstream,
			"Google 返回错误："+parsed.Error.Message).WithDetail("片段：%s", engine.Snippet(raw))
	}
	if len(parsed.Data.Translations) == 0 {
		return engine.Result{}, engine.New(engine.KindProtocol,
			"Google 返回了空结果（响应中没有 translations）").
			WithDetail("片段：%s", engine.Snippet(raw))
	}

	// Google 会把译文做 HTML 转义（' → &#39;），必须反转义
	target := html.UnescapeString(parsed.Data.Translations[0].TranslatedText)
	if strings.TrimSpace(target) == "" {
		return engine.Result{}, engine.New(engine.KindProtocol, "Google 返回了空译文")
	}

	detected := toCanonical(parsed.Data.Translations[0].DetectedSourceLanguage)

	return engine.Result{
		Target:       target,
		DetectedLang: detected,
		ElapsedMs:    time.Since(start).Milliseconds(),
		Chunks:       1,
		Engine:       engine.DisplayName(e.name, "Google 翻译"),
		Model:        "google-translate-v2",
	}, nil
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

func toCanonical(googleCode string) string {
	low := strings.ToLower(strings.TrimSpace(googleCode))
	if low == "" {
		return ""
	}
	for _, l := range lang.All() {
		if code, ok := lang.Map(Kind, l.Code); ok && strings.ToLower(code) == low {
			return l.Code
		}
	}
	return ""
}

// classifyAPIError 归类 Google 的错误响应。
//
// Google 用 400 表示大部分参数问题，但其中「API key not valid」是鉴权问题，
// 必须与「语言不支持」区分开 —— 前者让用户换 Key，后者让用户换语言。
func classifyAPIError(status int, message string, body []byte) error {
	low := strings.ToLower(message)

	switch {
	case status == http.StatusBadRequest && (strings.Contains(low, "api key") ||
		strings.Contains(low, "keyinvalid") || strings.Contains(low, "permission")):
		return engine.New(engine.KindAuth,
			"鉴权失败（400）：API Key 无效，或未启用 Cloud Translation API。请到 Google Cloud Console 检查").
			WithDetail("Google 说明：%s", engine.TruncateRunes(message, 200))
	case status == http.StatusBadRequest:
		base := "请求被拒绝（400）"
		if message != "" {
			base += "：" + message
		} else {
			base += "：可能是语言不支持或文本过长"
		}
		return engine.New(engine.KindConfig, base).WithDetail("片段：%s", engine.Snippet(body))
	case status == http.StatusForbidden:
		return engine.New(engine.KindAuth,
			"无权限（403）：API Key 有效但未启用 Cloud Translation API，或未开通结算").
			WithDetail("Google 说明：%s", engine.TruncateRunes(message, 200))
	case status == http.StatusTooManyRequests:
		return engine.New(engine.KindRateLimit,
			"请求过于频繁（429）：请稍后再试，或降低并发数与缩短分段间隔")
	}

	if kind, isErr := engine.ClassifyHTTPStatus(status); isErr {
		e := engine.New(kind, engine.StatusMessage(status))
		if message != "" {
			e.Message += "：" + message
		}
		return e.WithDetail("片段：%s", engine.Snippet(body))
	}
	return engine.Newf(engine.KindUpstream, "Google 返回意外状态码 %d", status).
		WithDetail("片段：%s", engine.Snippet(body))
}
