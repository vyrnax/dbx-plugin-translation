// Package tencent 实现腾讯云机器翻译（TMT TextTranslate）引擎。
//
// 签名算法为 TC3-HMAC-SHA256，分三层 HMAC 派生：
//
//	SecretDate    = HMAC-SHA256("TC3"+SecretKey, Date)
//	SecretService = HMAC-SHA256(SecretDate, "tmt")
//	SecretSigning = HMAC-SHA256(SecretService, "tc3_request")
//	Signature     = Hex(HMAC-SHA256(SecretSigning, StringToSign))
//
// X-TC-Timestamp 与实际时间偏差超过 5 分钟会直接签名失败。
package tencent

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/vyrnax/translation/engine"
	"github.com/vyrnax/translation/lang"
)

// Kind 是本引擎的类型标识。
const Kind = "tencent"

// maxCharsPerRequest 是单次请求的字符上限（腾讯 TMT 限制 2000 字符）。
const maxCharsPerRequest = 1800

const (
	defaultEndpoint = "https://tmt.tencentcloudapi.com"
	apiVersion      = "2018-03-21"
	service         = "tmt"
	algorithm       = "TC3-HMAC-SHA256"
	defaultRegion   = "ap-guangzhou"
)

const testProbeText = "hello"

func init() {
	engine.Register(Kind, build, schema())
}

type engineImpl struct {
	endpoint  string
	secretID  string
	secretKey string
	region    string
	projectID int
	client    *http.Client
	name      string
}

func build(cfg engine.Config, sec engine.Secrets, opts engine.Options) (engine.Engine, error) {
	secretID := strings.TrimSpace(engine.ConfigString(cfg, "secretId", ""))
	if secretID == "" {
		return nil, engine.New(engine.KindConfig, "腾讯云 SecretId 不能为空")
	}
	secretKey := engine.SecretValue(sec, "secretKey")
	if secretKey == "" {
		return nil, engine.New(engine.KindConfig, "腾讯云 SecretKey 不能为空")
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

	region := engine.ConfigString(cfg, "region", defaultRegion)
	if region == "" {
		region = defaultRegion
	}

	return &engineImpl{
		endpoint:  endpoint,
		secretID:  secretID,
		secretKey: secretKey,
		region:    region,
		projectID: engine.ConfigInt(cfg, "projectId", 0, 0, 1<<30),
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
	Response struct {
		Error *struct {
			Code    string `json:"Code"`
			Message string `json:"Message"`
		} `json:"Error"`
		RequestID  string `json:"RequestId"`
		Source     string `json:"Source"`
		Target     string `json:"Target"`
		SourceText string `json:"SourceText"`
		TargetText string `json:"TargetText"`
	} `json:"Response"`
}

func (e *engineImpl) Translate(ctx context.Context, r engine.Request) (engine.Result, error) {
	if err := engine.ValidateRequest(r); err != nil {
		return engine.Result{}, err
	}
	if n := len([]rune(r.Source)); n > maxCharsPerRequest {
		return engine.Result{}, engine.Newf(engine.KindConfig,
			"文本过长（%d 字符）：腾讯云翻译单次上限为 %d 字符。请分段翻译", n, maxCharsPerRequest)
	}
	start := time.Now()

	src := "auto"
	if r.SourceLang != "" && r.SourceLang != lang.Auto {
		code, ok := lang.MapSource(Kind, r.SourceLang)
		if !ok {
			return engine.Result{}, engine.Newf(engine.KindUnsupported,
				"腾讯云翻译不支持源语言 %s", lang.ZhName(r.SourceLang))
		}
		src = code
	}
	dst, ok := lang.MapTarget(Kind, r.TargetLang)
	if !ok {
		return engine.Result{}, engine.Newf(engine.KindUnsupported,
			"腾讯云翻译不支持目标语言 %s", lang.ZhName(r.TargetLang))
	}

	body := map[string]any{
		"SourceText": r.Source,
		"Source":     src,
		"Target":     dst,
		"ProjectId":  e.projectID,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return engine.Result{}, engine.Wrap(engine.KindConfig, "构造请求失败", err)
	}

	req, err := e.buildRequest(ctx, payload)
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
	if parsed.Response.Error != nil && parsed.Response.Error.Code != "" {
		return engine.Result{}, classifyErrorCode(parsed.Response.Error.Code, parsed.Response.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return engine.Result{}, classifyHTTPStatus(resp.StatusCode, raw)
	}

	target := parsed.Response.TargetText
	if strings.TrimSpace(target) == "" {
		return engine.Result{}, engine.New(engine.KindProtocol,
			"腾讯云翻译返回了空译文").WithDetail("片段：%s", engine.Snippet(raw))
	}

	return engine.Result{
		Target:       target,
		DetectedLang: toCanonical(parsed.Response.Source),
		ElapsedMs:    time.Since(start).Milliseconds(),
		Chunks:       1,
		Engine:       engine.DisplayName(e.name, "腾讯云翻译"),
		Model:        "tencent-tmt",
	}, nil
}

// buildRequest 构造带 TC3-HMAC-SHA256 签名的请求。
func (e *engineImpl) buildRequest(ctx context.Context, payload []byte) (*http.Request, error) {
	u, err := url.Parse(e.endpoint)
	if err != nil {
		return nil, engine.Wrap(engine.KindConfig, "接口地址非法", err)
	}
	host := u.Host
	// 腾讯云固定用 / 作为 CanonicalURI（API 挂在根上）
	canonicalURI := "/"
	contentType := "application/json; charset=utf-8"

	now := time.Now()
	timestamp := now.Unix()
	date := now.UTC().Format("2006-01-02")
	payloadHash := sha256Hex(payload)

	// 参与签名的头只有 content-type 与 host，必须与 SignedHeaders 完全一致
	signedHeaders := "content-type;host"
	canonicalHeaders := "content-type:" + contentType + "\n" + "host:" + host + "\n"

	// CanonicalQueryString 为空（本 API 无查询参数），对应位置保留空行
	canonicalRequest := http.MethodPost + "\n" +
		canonicalURI + "\n" +
		"\n" +
		canonicalHeaders + "\n" +
		signedHeaders + "\n" +
		payloadHash

	credentialScope := date + "/" + service + "/tc3_request"
	stringToSign := algorithm + "\n" +
		strconv.FormatInt(timestamp, 10) + "\n" +
		credentialScope + "\n" +
		sha256Hex([]byte(canonicalRequest))

	secretDate := hmacSHA256Raw([]byte("TC3"+e.secretKey), date)
	secretService := hmacSHA256Raw(secretDate, service)
	secretSigning := hmacSHA256Raw(secretService, "tc3_request")
	signature := hex.EncodeToString(hmacSHA256Raw(secretSigning, stringToSign))

	auth := algorithm + " Credential=" + e.secretID + "/" + credentialScope +
		", SignedHeaders=" + signedHeaders +
		", Signature=" + signature

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, engine.Wrap(engine.KindConfig, "构造请求失败", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", auth)
	req.Header.Set("X-TC-Action", "TextTranslate")
	req.Header.Set("X-TC-Version", apiVersion)
	req.Header.Set("X-TC-Timestamp", strconv.FormatInt(timestamp, 10))
	if e.region != "" {
		req.Header.Set("X-TC-Region", e.region)
	}
	req.Host = host
	return req, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256Raw(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
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

func toCanonical(code string) string {
	low := strings.ToLower(strings.TrimSpace(code))
	if low == "" || low == "auto" {
		return ""
	}
	for _, l := range lang.All() {
		if c, ok := lang.Map(Kind, l.Code); ok && strings.ToLower(c) == low {
			return l.Code
		}
	}
	return ""
}

// classifyErrorCode 把腾讯云的业务错误码映射为统一错误类型。
//
// 腾讯云错误码形如 "AuthFailure.SignatureFailure"、"RequestLimitExceeded"。
func classifyErrorCode(code, msg string) error {
	detail := msg
	if detail == "" {
		detail = "（腾讯云未返回错误描述）"
	}
	low := strings.ToLower(code + " " + detail)

	switch {
	case strings.Contains(low, "authfailure.secretidnotfound"),
		strings.Contains(low, "authfailure.signaturefailure"),
		strings.Contains(low, "authfailure.invalidsignature"),
		strings.Contains(low, "invalidsecretid"):
		return engine.New(engine.KindAuth,
			"鉴权失败（"+code+"）：请检查 SecretId/SecretKey 是否正确，以及本机时间是否准确").
			WithDetail("腾讯云说明：%s", detail)
	case strings.Contains(low, "authfailure"), strings.Contains(low, "unauthorizedoperation"):
		return engine.New(engine.KindAuth,
			"无权限（"+code+"）：请在腾讯云 CAM 控制台为该账号授予「机器翻译」权限").
			WithDetail("腾讯云说明：%s", detail)
	case strings.Contains(low, "requestlimitexceeded"), strings.Contains(low, "limitsexceeded"):
		return engine.New(engine.KindRateLimit,
			"请求超过频率限制（"+code+"）：请降低并发数或增大分段间隔").
			WithDetail("腾讯云说明：%s", detail)
	case strings.Contains(low, "resourceunavailable"), strings.Contains(low, "insufficientbalance"),
		strings.Contains(low, "resourcesoldout"):
		return engine.New(engine.KindRateLimit,
			"资源不可用或余额不足（"+code+"）：请到腾讯云控制台检查余额与套餐").
			WithDetail("腾讯云说明：%s", detail)
	case strings.Contains(low, "invalidparameter"), strings.Contains(low, "missingparameter"):
		return engine.New(engine.KindConfig,
			"参数非法（"+code+"）：请检查语言与文本设置").
			WithDetail("腾讯云说明：%s", detail)
	case strings.Contains(low, "unsupportedoperation"), strings.Contains(low, "unsupportedlanguage"):
		return engine.New(engine.KindUnsupported,
			"不支持的操作或语言（"+code+"）：请换一个语言").
			WithDetail("腾讯云说明：%s", detail)
	case strings.Contains(low, "internalerror"), strings.Contains(low, "serviceunavailable"):
		return engine.New(engine.KindUpstream,
			"腾讯云服务暂时不可用（"+code+"）：请稍后重试").
			WithDetail("腾讯云说明：%s", detail)
	}
	return engine.Newf(engine.KindUpstream, "腾讯云翻译返回错误 %s：%s", code, detail)
}

func classifyHTTPStatus(status int, body []byte) error {
	// 腾讯云正常业务错误也返回 200，走到这里说明是网关层错误
	if kind, isErr := engine.ClassifyHTTPStatus(status); isErr {
		return engine.New(kind, engine.StatusMessage(status)).
			WithDetail("片段：%s", engine.Snippet(body))
	}
	return engine.Newf(engine.KindUpstream, "腾讯云翻译返回意外状态码 %d", status).
		WithDetail("片段：%s", engine.Snippet(body))
}
