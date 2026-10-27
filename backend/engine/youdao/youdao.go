// Package youdao 实现有道智云翻译引擎（API v3）。
//
// # 签名算法（SHA256）
//
//	signStr = appKey + truncate(q) + salt + curtime + appSecret
//	sign    = SHA256(signStr) 的十六进制小写
//
// 其中 truncate 是【最容易写错】的一步：
//
//	若 len(q) <= 20  →  q 原样
//	若 len(q) >  20  →  q[:10] + len(q) + q[-10:]
//
// 关键：len(q) 按【字符数（rune）】而非字节数。中文一个字 3 字节，
// 用 len(string) 会得到字节数，导致中文文本的签名全部错误 ——
// 症状是「英文能翻、中文一直 110 签名无效」，极难排查。
//
// 另外：这里的 len(q) 用的是【原文长度】，不是截断后的长度。
//
// # errorCode 也是字符串
//
// 与百度一样，有道响应体里的 errorCode 是 JSON 字符串（"0" / "108"）。
package youdao

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
const Kind = "youdao"

// maxCharsPerRequest 是单次请求的字符上限。
//
// 有道 v3 单次上限 5000 字符（按字符计，不是字节）。保守取 4500 留余量。
const maxCharsPerRequest = 4500

const defaultEndpoint = "https://openapi.youdao.com/api"

const testProbeText = "hello"

func init() {
	engine.Register(Kind, build, schema())
}

type engineImpl struct {
	endpoint  string
	appKey    string
	appSecret string
	client    *http.Client
	name      string
}

func build(cfg engine.Config, sec engine.Secrets, opts engine.Options) (engine.Engine, error) {
	appKey := strings.TrimSpace(engine.ConfigString(cfg, "appKey", ""))
	if appKey == "" {
		return nil, engine.New(engine.KindConfig, "有道应用 ID（App Key）不能为空")
	}
	appSecret := engine.SecretValue(sec, "appSecret")
	if appSecret == "" {
		return nil, engine.New(engine.KindConfig, "有道应用密钥（App Secret）不能为空")
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
		endpoint:  endpoint,
		appKey:    appKey,
		appSecret: appSecret,
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

// truncate 实现有道 v3 的输入截断规则。
//
// 必须按【rune】计数：中文一个字 3 字节，用 len(string) 会得到字节数，
// 导致中文文本签名全部错误（症状：英文能翻、中文报 110 签名无效）。
func truncate(q string) string {
	r := []rune(q)
	n := len(r)
	if n <= 20 {
		return q
	}
	// 注意中间的 len 用的是【原文总字符数】，不是截断后的长度
	return string(r[:10]) + strconv.Itoa(n) + string(r[n-10:])
}

type translateResponse struct {
	ErrorCode   string   `json:"errorCode"`
	Translation []string `json:"translation"`
	Query       string   `json:"query"`
	L           string   `json:"l"` // 语言方向，如 "en2zh-CHS"
}

func (e *engineImpl) Translate(ctx context.Context, r engine.Request) (engine.Result, error) {
	if err := engine.ValidateRequest(r); err != nil {
		return engine.Result{}, err
	}
	if n := len([]rune(r.Source)); n > maxCharsPerRequest {
		return engine.Result{}, engine.Newf(engine.KindConfig,
			"文本过长（%d 字符）：有道翻译单次上限为 %d 字符。请分段翻译", n, maxCharsPerRequest)
	}
	start := time.Now()

	// 有道【不支持】源语言自动检测，必须显式给出
	if r.SourceLang == "" || r.SourceLang == lang.Auto {
		return engine.Result{}, engine.New(engine.KindUnsupported,
			"有道翻译不支持自动检测源语言，请在翻译页显式选择源语言")
	}
	from, ok := lang.MapSource(Kind, r.SourceLang)
	if !ok {
		return engine.Result{}, engine.Newf(engine.KindUnsupported,
			"有道翻译不支持源语言 %s", lang.ZhName(r.SourceLang))
	}
	to, ok := lang.MapTarget(Kind, r.TargetLang)
	if !ok {
		return engine.Result{}, engine.Newf(engine.KindUnsupported,
			"有道翻译不支持目标语言 %s", lang.ZhName(r.TargetLang))
	}

	salt := fmt.Sprintf("%d", time.Now().UnixNano()/1e6)
	curtime := strconv.FormatInt(time.Now().Unix(), 10)
	signStr := e.appKey + truncate(r.Source) + salt + curtime + e.appSecret
	sum := sha256.Sum256([]byte(signStr))
	sign := hex.EncodeToString(sum[:])

	form := url.Values{}
	form.Set("q", r.Source)
	form.Set("from", from)
	form.Set("to", to)
	form.Set("appKey", e.appKey)
	form.Set("salt", salt)
	form.Set("sign", sign)
	form.Set("signType", "v3")
	form.Set("curtime", curtime)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return engine.Result{}, engine.Wrap(engine.KindConfig, "构造请求失败", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

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

	if parsed.ErrorCode != "" && parsed.ErrorCode != "0" {
		return engine.Result{}, classifyErrorCode(parsed.ErrorCode)
	}
	if resp.StatusCode != http.StatusOK {
		return engine.Result{}, classifyHTTPStatus(resp.StatusCode, raw)
	}
	if len(parsed.Translation) == 0 {
		return engine.Result{}, engine.New(engine.KindProtocol,
			"有道翻译返回了空结果（响应中没有 translation）").
			WithDetail("片段：%s", engine.Snippet(raw))
	}

	target := strings.Join(parsed.Translation, "")
	if strings.TrimSpace(target) == "" {
		return engine.Result{}, engine.New(engine.KindProtocol, "有道翻译返回了空译文")
	}

	return engine.Result{
		Target:       target,
		DetectedLang: parseLangDirection(parsed.L),
		ElapsedMs:    engine.ElapsedMs(start),
		Chunks:       1,
		Engine:       engine.DisplayName(e.name, "有道翻译"),
		Model:        "youdao-v3",
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

// parseLangDirection 从有道的 "en2zh-CHS" 形式里取出源语言并转规范码。
func parseLangDirection(l string) string {
	i := strings.Index(l, "2")
	if i <= 0 {
		return ""
	}
	src := strings.ToLower(strings.TrimSpace(l[:i]))
	for _, c := range lang.All() {
		if code, ok := lang.Map(Kind, c.Code); ok && strings.ToLower(code) == src {
			return c.Code
		}
	}
	return ""
}

// classifyErrorCode 把有道的业务错误码映射为统一错误类型。
//
// 有道错误码是纯数字字符串，且不少错误码的含义与其它服务商不同：
//   - 108 是「应用 ID 无效」而不是「余额不足」
//   - 109 是「批量接口请求频次受限」
//   - 110 是「无相关服务」而非「签名错误」（签名错误是 202）
//   - 202 是「签名校验失败」
//   - 411 是「访问频率受限」
//   - 401 是「账户已欠费」
func classifyErrorCode(code string) error {
	switch code {
	case "101": // 缺少必填参数
		return engine.New(engine.KindConfig, "缺少必填参数（101）：请检查文本与语言设置")
	case "102": // 不支持的语言类型
		return engine.New(engine.KindUnsupported, "不支持的语言类型（102）：请换一个语言")
	case "103": // 翻译文本过长
		return engine.New(engine.KindConfig, "翻译文本过长（103）：请缩短文本或调低「单次请求字符上限」")
	case "104": // 不支持的 API 类型
		return engine.New(engine.KindConfig, "不支持的 API 类型（104）：请确认接口地址为 .../api")
	case "105": // 不支持的签名类型
		return engine.New(engine.KindConfig, "不支持的签名类型（105）：请确认使用 v3 签名")
	case "106": // 不支持的响应类型
		return engine.New(engine.KindConfig, "不支持的响应类型（106）")
	case "108": // 应用 ID 无效
		return engine.New(engine.KindAuth,
			"应用 ID 无效（108）：请检查「App Key」是否填写正确，或该应用是否已绑定「文本翻译」服务")
	case "109": // 批量请求超出频次
		return engine.New(engine.KindRateLimit,
			"请求频次超限（109）：请降低并发数与缩短分段间隔")
	case "110": // 无相关服务
		return engine.New(engine.KindConfig,
			"无相关服务（110）：请在有道智云控制台为当前应用开通「文本翻译」服务")
	case "111": // 开发者账号无效
		return engine.New(engine.KindAuth, "开发者账号无效（111）：请检查有道智云账号状态")
	case "112": // 请求服务无效
		return engine.New(engine.KindConfig, "请求服务无效（112）：请检查接口地址")
	case "113": // 传参不规范
		return engine.New(engine.KindConfig, "传参不规范（113）：请检查文本与语言是否符合要求")
	case "201": // 解密失败
		return engine.New(engine.KindProtocol, "响应解密失败（201）")
	case "202": // 签名校验失败
		return engine.New(engine.KindAuth,
			"签名校验失败（202）：通常是应用密钥（App Secret）不正确")
	case "203": // 访问 IP 不在可访问 IP 列表
		return engine.New(engine.KindConfig,
			"访问 IP 不在白名单（203）：请在有道智云控制台把当前出口 IP 加入可访问 IP 列表")
	case "205": // 请求翻译文本超过限制
		return engine.New(engine.KindConfig, "请求文本超过限制（205）：请缩短文本")
	case "206": // 时间戳错误（偏差超过 5 分钟）
		return engine.New(engine.KindConfig,
			"时间戳错误（206）：本机时间与标准时间偏差超过 5 分钟，请校准系统时间")
	case "207": // 重放请求
		return engine.New(engine.KindNetwork, "重放请求（207）：请重试")
	case "401": // 账户已欠费
		return engine.New(engine.KindRateLimit,
			"账户已欠费（401）：请到有道智云控制台充值")
	case "411": // 访问频率受限
		return engine.New(engine.KindRateLimit,
			"访问频率受限（411）：请降低并发数或增大分段间隔")
	case "412": // 请求量超出限制
		return engine.New(engine.KindRateLimit, "请求量超出限制（412）：请稍后重试")
	case "413": // 长请求频繁
		return engine.New(engine.KindRateLimit, "长文本请求过于频繁（413）：请增大分段间隔")
	case "414": // 单次请求超过限制
		return engine.New(engine.KindConfig, "单次请求超过限制（414）：请缩短文本")
	}
	return engine.Newf(engine.KindUpstream, "有道翻译返回错误码 %s", code)
}

func classifyHTTPStatus(status int, body []byte) error {
	if kind, isErr := engine.ClassifyHTTPStatus(status); isErr {
		e := engine.New(kind, engine.StatusMessage(status))
		if msg := engine.ExtractJSONMessage(body); msg != "" {
			e.Message += "：" + msg
		}
		return e.WithDetail("片段：%s", engine.Snippet(body))
	}
	return engine.Newf(engine.KindUpstream, "有道翻译返回意外状态码 %d", status).
		WithDetail("片段：%s", engine.Snippet(body))
}
