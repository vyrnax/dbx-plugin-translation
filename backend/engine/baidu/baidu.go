// Package baidu 实现百度翻译开放平台（通用文本翻译）引擎。
//
// # 签名算法（MD5）
//
//	sign = MD5(appid + q + salt + secretKey)
//
// 其中 salt 是随机数（官方示例用时间戳，但要求「随机数」，用时间戳
// 在高并发下可能重复，因此这里用纳秒时间戳 + 随机数混合）。
// 拼接顺序【必须】严格是 appid → q → salt → secretKey，顺序错了签名必错，
// 而错误码 54001 只写「签名错误」，不会告诉你是顺序问题。
//
// # error_code 是【字符串】
//
// 百度的响应体里 error_code 是 JSON 字符串（"52003"）而不是数字（52003）。
// 用 int 字段解析会得到 0，导致所有错误都被当成成功 —— 这是很隐蔽的坑。
package baidu

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gaogao/translation/engine"
	"github.com/gaogao/translation/lang"
)

// Kind 是本引擎的类型标识。
const Kind = "baidu"

// maxBytesPerRequest 是百度单次请求的上限（按 UTF-8 字节计）。
//
// 官方限制 6000 字节。这里按 【字节】而非字符判断，因为百度就是按字节算的
// —— 用字符数判断会让中文长文本（每字 3 字节）实际超限 3 倍。
const maxBytesPerRequest = 6000

// maxCharsPerRequest 是对外的字符上限（保守估算：中文 1 字符 3 字节）。
//
// 实际校验在 Translate 里按字节做，这个值只用于调用方的分段决策。
const maxCharsPerRequest = 2000

const defaultEndpoint = "https://fanyi-api.baidu.com/api/trans/vip/translate"

const testProbeText = "hello"

func init() {
	engine.Register(Kind, build, schema())
}

type engineImpl struct {
	endpoint  string
	appID     string
	secretKey string
	client    *http.Client
	name      string
}

func build(cfg engine.Config, sec engine.Secrets, opts engine.Options) (engine.Engine, error) {
	appID := strings.TrimSpace(engine.ConfigString(cfg, "appId", ""))
	if appID == "" {
		return nil, engine.New(engine.KindConfig, "百度翻译 AppID 不能为空")
	}
	secretKey := engine.SecretValue(sec, "secretKey")
	if secretKey == "" {
		return nil, engine.New(engine.KindConfig, "百度翻译密钥（Secret Key）不能为空")
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
		appID:     appID,
		secretKey: secretKey,
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
	From        string `json:"from"`
	To          string `json:"to"`
	TransResult []struct {
		Src string `json:"src"`
		Dst string `json:"dst"`
	} `json:"trans_result"`
	// 注意：百度的 error_code 是【字符串】。用 int 解析会得到 0，
	// 导致所有错误都被静默当成成功。
	ErrorCode string `json:"error_code"`
	ErrorMsg  string `json:"error_msg"`
}

func (e *engineImpl) Translate(ctx context.Context, r engine.Request) (engine.Result, error) {
	if err := engine.ValidateRequest(r); err != nil {
		return engine.Result{}, err
	}
	// 百度按【字节】计上限，必须在发请求前算，不能等上游返回 54004
	if n := len(r.Source); n > maxBytesPerRequest {
		return engine.Result{}, engine.Newf(engine.KindConfig,
			"文本过长（%d 字节）：百度翻译单次上限为 %d 字节（中文约 %d 字）。请分段翻译",
			n, maxBytesPerRequest, maxBytesPerRequest/3)
	}
	start := time.Now()

	var from string
	if r.SourceLang == "" || r.SourceLang == lang.Auto {
		from = "auto"
	} else {
		code, ok := lang.MapSource(Kind, r.SourceLang)
		if !ok {
			return engine.Result{}, engine.Newf(engine.KindUnsupported,
				"百度翻译不支持源语言 %s", lang.ZhName(r.SourceLang))
		}
		from = code
	}
	to, ok := lang.MapTarget(Kind, r.TargetLang)
	if !ok {
		return engine.Result{}, engine.Newf(engine.KindUnsupported,
			"百度翻译不支持目标语言 %s", lang.ZhName(r.TargetLang))
	}

	// salt 是随机数。官方示例用时间戳，但高并发下时间戳可能重复，
	// 重复的 salt 会让两次请求的签名相同（虽然不致命，但削弱了防重放的意义）。
	// 这里用「纳秒时间戳 + 随机数」保证唯一。
	salt := fmt.Sprintf("%d%04d", time.Now().UnixNano(), rand.Intn(10000))

	// 签名严格按 appid → q → salt → secretKey 顺序拼接
	signStr := e.appID + r.Source + salt + e.secretKey
	sum := md5.Sum([]byte(signStr))
	sign := hex.EncodeToString(sum[:])

	form := url.Values{}
	form.Set("q", r.Source)
	form.Set("from", from)
	form.Set("to", to)
	form.Set("appid", e.appID)
	form.Set("salt", salt)
	form.Set("sign", sign)

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

	// 业务错误：百度用 200 + error_code 表示失败
	if parsed.ErrorCode != "" && parsed.ErrorCode != "0" {
		return engine.Result{}, classifyErrorCode(parsed.ErrorCode, parsed.ErrorMsg)
	}

	if resp.StatusCode != http.StatusOK {
		return engine.Result{}, classifyHTTPStatus(resp.StatusCode, raw)
	}

	if len(parsed.TransResult) == 0 {
		return engine.Result{}, engine.New(engine.KindProtocol,
			"百度翻译返回了空结果（响应中没有 trans_result）").
			WithDetail("片段：%s", engine.Snippet(raw))
	}

	// 百度会把长文本按句拆成多个 trans_result 项，必须全部拼接
	var parts []string
	for _, item := range parsed.TransResult {
		parts = append(parts, item.Dst)
	}
	target := strings.Join(parts, "")
	if strings.TrimSpace(target) == "" {
		return engine.Result{}, engine.New(engine.KindProtocol, "百度翻译返回了空译文")
	}

	return engine.Result{
		Target:       target,
		DetectedLang: fromCanonical(parsed.From),
		ElapsedMs:    time.Since(start).Milliseconds(),
		Chunks:       1,
		Engine:       engine.DisplayName(e.name, "百度翻译"),
		Model:        "baidu-translate",
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

func fromCanonical(baiduCode string) string {
	low := strings.ToLower(strings.TrimSpace(baiduCode))
	if low == "" || low == "auto" {
		return ""
	}
	for _, l := range lang.All() {
		if code, ok := lang.Map(Kind, l.Code); ok && strings.ToLower(code) == low {
			return l.Code
		}
	}
	return ""
}

// classifyErrorCode 把百度的业务错误码映射为统一的错误类型。
//
// 完整的错误码表见官方文档；这里覆盖最常见、且处理方式不同的几类：
//   - 52001/52002/52003：鉴权类 → 不重试，提示检查配置
//   - 54003/54005：限流类 → 不重试，提示稍后
//   - 54004：账户余额不足 → 限流类（配额问题）
//   - 58000/58001/58002：客户端 IP 或权限问题
//   - 54001：签名错误 → 鉴权类（通常是密钥错或拼接顺序错）
func classifyErrorCode(code, msg string) error {
	detail := msg
	if detail == "" {
		detail = "（百度未返回错误描述）"
	}
	switch code {
	case "52001": // 请求超时
		return engine.New(engine.KindNetwork, "百度翻译请求超时（52001），请重试").
			WithDetail("百度说明：%s", detail)
	case "52002": // 系统错误
		return engine.New(engine.KindUpstream, "百度翻译系统错误（52002），请稍后重试").
			WithDetail("百度说明：%s", detail)
	case "52003": // 未授权用户
		return engine.New(engine.KindAuth,
			"鉴权失败（52003）：AppID 或密钥无效。请检查「AppID」与「密钥」是否填反，或在百度翻译开放平台确认账号已开通通用文本翻译").
			WithDetail("百度说明：%s", detail)
	case "54000": // 必填参数为空
		return engine.New(engine.KindConfig, "必填参数为空（54000）：请检查文本与语言设置").
			WithDetail("百度说明：%s", detail)
	case "54001": // 签名错误
		return engine.New(engine.KindAuth,
			"签名错误（54001）：通常是密钥不正确。若确认密钥无误，请检查账号是否为「个人开发者」并已开通通用文本翻译").
			WithDetail("百度说明：%s", detail)
	case "54003": // 访问频率受限
		return engine.New(engine.KindRateLimit,
			"访问频率受限（54003）：请降低并发数或增大分段间隔（标准版免费额度为 1 QPS）").
			WithDetail("百度说明：%s", detail)
	case "54004": // 账户余额不足
		return engine.New(engine.KindRateLimit,
			"账户余额不足（54004）：免费额度已用完，请到百度翻译开放平台充值或等待下月重置").
			WithDetail("百度说明：%s", detail)
	case "54005": // 长 query 请求频繁
		return engine.New(engine.KindRateLimit,
			"长文本请求过于频繁（54005）：请增大分段间隔后重试").
			WithDetail("百度说明：%s", detail)
	case "58000": // 客户端 IP 非法
		return engine.New(engine.KindConfig,
			"客户端 IP 非法（58000）：请在百度翻译开放平台把当前出口 IP 加入白名单，或关闭 IP 限制").
			WithDetail("百度说明：%s", detail)
	case "58001": // 译文语言方向不支持
		return engine.New(engine.KindUnsupported,
			"该语言方向不受支持（58001）：请换一个目标语言，或在百度平台确认已开通该语种").
			WithDetail("百度说明：%s", detail)
	case "58002": // 服务当前已关闭
		return engine.New(engine.KindConfig,
			"服务当前已关闭（58002）：请在百度翻译开放平台重新开启通用文本翻译服务").
			WithDetail("百度说明：%s", detail)
	case "90107": // 认证未通过或未生效
		return engine.New(engine.KindAuth,
			"认证未通过或未生效（90107）：请在百度翻译开放平台完成开发者认证").
			WithDetail("百度说明：%s", detail)
	}
	// 未知错误码：保守归为上游错误（可重试），并带上原始码便于排查
	return engine.Newf(engine.KindUpstream, "百度翻译返回错误 %s：%s", code, detail)
}

func classifyHTTPStatus(status int, body []byte) error {
	if kind, isErr := engine.ClassifyHTTPStatus(status); isErr {
		e := engine.New(kind, engine.StatusMessage(status))
		if msg := engine.ExtractJSONMessage(body); msg != "" {
			e.Message += "：" + msg
		}
		return e.WithDetail("片段：%s", engine.Snippet(body))
	}
	return engine.Newf(engine.KindUpstream, "百度翻译返回意外状态码 %d", status).
		WithDetail("片段：%s", engine.Snippet(body))
}

// 避免 strconv 未使用（保留给未来可能的数值型错误码解析）
var _ = strconv.Itoa
