// Package openai 实现 OpenAI 兼容的 Chat Completions 引擎。
//
// # 为什么一个适配器能覆盖这么多服务商
//
// DeepSeek、通义千问、智谱、Kimi、Moonshot、Ollama、vLLM、LM Studio、
// 以及 OpenAI 自己，都实现了同一套 `/v1/chat/completions` 协议。
// 用户只需填「完整端点 URL + 模型名 + API Key」，就能接入其中任何一个。
// 这是本插件覆盖面最广的引擎类型。
//
// # 端点地址的处理
//
// 用户填写习惯极不统一，有人填 `https://api.deepseek.com`，
// 有人填 `https://api.deepseek.com/v1`，有人填完整的
// `https://api.deepseek.com/v1/chat/completions`。
// 三种都必须能用，否则用户会反复遇到 404 而不知道为什么。
// 因此 normalizeEndpoint 会智能补全路径（见该函数注释）。
//
// # 流式解析
//
// SSE（Server-Sent Events）的解析有几个容易踩的坑，sseScanner 逐一处理：
//   - 一行 `data:` 可能被 TCP 分片拆成多次 read，必须按行缓冲
//   - 一个事件可能有多行 `data:`，需要用空行分隔后再合并
//   - 心跳注释行以 `:` 开头，必须忽略而不是当成 JSON 解析
//   - 结束标记是 `data: [DONE]`，不是 JSON，直接解析会报错
//   - 某些服务商在最后一个 chunk 后才发 usage，不能提前关闭
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/vyrnax/translation/engine"
	"github.com/vyrnax/translation/lang"
)

// Kind 是本引擎的类型标识，与 Register 时的 kind 一致。
const Kind = "openai"

// defaultMaxChars 是单次请求的默认字符上限。
//
// 20000 字符约等于 10000-15000 token（中文约 1.5 字符/token），
// 加上系统 Prompt 与译文输出，总消耗约 30000 token。
// 这对 32K 上下文的模型是安全的，对 8K 模型则偏高 ——
// 因此该值可通过表单的 maxChars 字段覆盖。
const defaultMaxChars = 20000

// defaultTemperature 是采样温度。
//
// 0.3 而非 0：翻译需要一定的语言流畅度，temperature=0 的贪心解码
// 在中文里容易产生重复短语。0.3 在「忠实」与「流畅」之间平衡较好。
const defaultTemperature = 0.3

// testProbeText 是连通性探测用的文本。
//
// 刻意选最短的常见词：探测的目的是验证「端点可达 + Key 有效 + 模型存在」，
// 不是验证翻译质量。用短文本能把探测的 token 消耗和耗时都压到最低。
const testProbeText = "hello"

func init() {
	engine.Register(Kind, New, schema())
}

// Config 是本引擎的配置项（从 engine.Config 解析而来）。
type Config struct {
	Endpoint       string
	Model          string
	TimeoutSecs    int
	MaxChars       int
	Temperature    float64
	SkipTLSVerify  bool
	Concurrency    int
	PromptTemplate string
	SystemPrompt   string
	ExtraHeaders   map[string]string
}

// engineImpl 是 Engine 接口的实现。
type engineImpl struct {
	cfg    Config
	apiKey string
	client *http.Client
	name   string
}

// New 是注册到 engine 注册表的工厂函数，【必须导出】：
// 内置免费引擎（engine/free）复用的就是这份实现 —— 它对接的
// B站 Index-Translate 走的同样是 OpenAI Chat Completions 协议，
// SSE 解析、提示词模板、译文清洗、错误分类都已经在包里调通并测过。
// 不导出就只能再复制一份协议实现，而「两份实现逐渐漂移」正是本项目反复踩过的坑。
func New(cfg engine.Config, sec engine.Secrets, opts engine.Options) (engine.Engine, error) {
	endpoint, warning, err := engine.ValidateEndpoint(engine.ConfigString(cfg, "endpoint", ""))
	if err != nil {
		return nil, err
	}
	normalized, err := normalizeEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	model := strings.TrimSpace(engine.ConfigString(cfg, "model", ""))
	if model == "" {
		return nil, engine.New(engine.KindConfig, "模型名不能为空")
	}
	noAuth := engine.ConfigBool(cfg, "noAuth", false)
	apiKey := engine.SecretValue(sec, "apiKey")
	if apiKey == "" && !noAuth {
		// 某些本地服务（Ollama）不需要 Key，因此这里只警告不报错。
		// 但如果端点不是 localhost，缺 Key 几乎肯定是配置遗漏。
		if !engine.IsLocalhost(mustHostname(normalized)) {
			return nil, engine.New(engine.KindConfig, "API Key 不能为空")
		}
	}
	// noAuth 是【内部】开关：本包的 schema 里没有这个字段，用户在表单上填不出来，
	// 只有内置引擎的锁定配置会带上它。存在的理由：那个公益端点既不需要凭据、
	// 又不是 localhost —— 没有这个出口就只能塞一个假 Key，
	// 而假 Key 会被当作 Authorization 头发出去，反而可能被服务端拒绝。

	c := Config{
		Endpoint:    normalized,
		Model:       model,
		TimeoutSecs: engine.ConfigInt(cfg, "timeoutSecs", engine.DefaultTimeoutSecs, 5, 600),
		// 下限从 500 降到 100：内置免费引擎的服务端有 30 秒网关硬超时，
		// 实测单段 820 字符就会 504，安全上限只有 400 —— 原来的 500 下限
		// 会让它【无法】表达自己真正需要的分段粒度（用户侧同样受益：
		// 某些小上下文模型本来就只吃得下几百字符）。
		MaxChars:       engine.ConfigInt(cfg, "maxChars", defaultMaxChars, 100, 100000),
		Temperature:    configFloat(cfg, "temperature", defaultTemperature, 0, 2),
		SkipTLSVerify:  engine.ConfigBool(cfg, "skipTlsVerify", false),
		Concurrency:    engine.ConfigInt(cfg, "concurrency", 1, 1, 4),
		PromptTemplate: engine.ConfigString(cfg, "promptTemplate", ""),
		SystemPrompt:   engine.ConfigString(cfg, "systemPrompt", ""),
		ExtraHeaders:   parseExtraHeaders(engine.ConfigString(cfg, "extraHeaders", "")),
	}

	_ = warning // 警告由 RPC 层在保存时返回给 UI，这里不重复处理

	return &engineImpl{
		cfg:    c,
		apiKey: apiKey,
		client: engine.NewHTTPClient(c.TimeoutSecs, c.SkipTLSVerify),
		name:   opts.Name,
	}, nil
}

func (e *engineImpl) Kind() string             { return Kind }
func (e *engineImpl) SupportsStreaming() bool  { return true }
func (e *engineImpl) MaxChars() int            { return e.cfg.MaxChars }
func (e *engineImpl) SupportedLangs() []string { return lang.SupportedLangs(Kind) }

// ---------------------------------------------------------------------------
// 端点归一化
// ---------------------------------------------------------------------------

// normalizeEndpoint 把用户填写的各种形态统一成完整的 chat/completions URL。
//
// 支持的输入形态：
//
//	https://api.deepseek.com
//	https://api.deepseek.com/
//	https://api.deepseek.com/v1
//	https://api.deepseek.com/v1/
//	https://api.deepseek.com/v1/chat/completions
//	https://api.openai.com/v1/chat/completions?api-version=2024-01-01
//
// 全部归一化为 `.../v1/chat/completions`（保留原有 query）。
//
// 为什么必须做这件事：用户填错端点路径是最高频的配置错误，
// 而症状是 404，错误消息通常是服务商返回的 HTML 页面，
// 用户完全看不出是路径问题。自动补全能消掉这一整类问题。
func normalizeEndpoint(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", engine.New(engine.KindConfig, "接口地址不能为空")
	}
	// 分离 query（Azure OpenAI 等需要 api-version 参数）
	query := ""
	if i := strings.IndexByte(s, '?'); i >= 0 {
		query = s[i:]
		s = s[:i]
	}
	s = strings.TrimRight(s, "/")

	switch {
	case strings.HasSuffix(s, "/chat/completions"):
		// 已经是完整路径
	case strings.HasSuffix(s, "/completions"):
		// 老式 completions 端点（非 chat），保持原样
	case strings.HasSuffix(s, "/v1"):
		s += "/chat/completions"
	case strings.Contains(s, "/v1/"):
		// 形如 .../v1/something，补成 .../v1/chat/completions
		i := strings.Index(s, "/v1/")
		s = s[:i] + "/v1/chat/completions"
	default:
		// 只有主机名，补全标准路径
		s += "/v1/chat/completions"
	}
	return s + query, nil
}

// mustHostname 从 URL 里取主机名，失败返回空串。
func mustHostname(rawURL string) string {
	// 不引入 net/url 的额外解析：这里只需要粗略判断是否 localhost
	s := rawURL
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	return s
}

// ---------------------------------------------------------------------------
// 请求构造
// ---------------------------------------------------------------------------

// chatRequest 是 Chat Completions 的请求体。
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	Temperature float64       `json:"temperature"`
	// MaxTokens 刻意不设：翻译的输出长度与输入相关，
	// 设小了会截断译文，设大了浪费配额。让服务商按模型默认值处理。
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// defaultSystemPrompt 是内置的系统提示词。
//
// 每一句都对应一个真实发生过的问题：
//   - 「只输出译文」：不加这句，模型会输出「译文：xxx」或附带解释
//   - 「不要引号」：模型喜欢把译文包在引号里，导致结果需要后处理剥离
//   - 「保留换行与缩进」：代码注释、配置文件的翻译必须保持结构
//   - 「保留占位符」：{name}、%s 这类占位符被翻译掉就无法回填了
//   - 「代码块原样保留」：翻译技术文档时，代码被"翻译"会直接毁掉文档
//
// defaultSystemPrompt 是内置的系统提示词。
//
// 每一句都对应一个真实发生过的问题：
//   - 「只输出译文」：不加这句，模型会输出「译文：xxx」或附带解释
//   - 「不要引号」：模型喜欢把译文包在引号里，导致结果需要后处理剥离
//   - 「保留换行与缩进」：代码注释、配置文件的翻译必须保持结构
//   - 「保留占位符」：{name}、%s 这类占位符被翻译掉就无法回填了
//   - 「代码块原样保留」：翻译技术文档时，代码被"翻译"会直接毁掉文档
//
// 注意这里用了「原始字符串 + 解释字符串」的拼接写法：
// Go 的原始字符串（反引号包裹）内部【不能】出现反引号，
// 而 Prompt 需要提到 ``` 围栏与 ` 行内代码，因此必须拆开拼接。
const defaultSystemPrompt = `你是一个专业的翻译引擎。请把用户提供的文本翻译成{targetLang}。

严格遵守以下规则：
1. 只输出译文本身，不要输出任何解释、说明、前言或结语。
2. 不要给译文加引号或任何包裹符号。
3. 完整保留原文的换行、缩进、空行与 Markdown 标记。
4. 完整保留占位符（如 {name}、%s、%d、${var}）与代码标识符，不要翻译它们。
5. 代码块（` + "```" + ` 围栏内）与行内代码（` + "`" + ` 包裹）的内容保持原样，不要翻译。
6. 专有名词、产品名、API 名称保持原文。
7. 如果原文已经是{targetLang}，直接原样输出。
8. 保留原文的大小写：全大写词（如 US、ID、HTTP、JSON）往往是语义的一部分（US=美国、us=我们；ID=标识、it=它），不要擅自改成小写或展开成全称。`

// buildMessages 构造对话消息。
func (e *engineImpl) buildMessages(r engine.Request) []chatMessage {
	targetName := lang.LLMName(r.TargetLang)
	sourceName := lang.LLMName(r.SourceLang)

	tmpl := strings.TrimSpace(e.cfg.PromptTemplate)
	system := e.cfg.SystemPrompt
	switch {
	case tmpl != "":
		// 用户自定义模板：支持 {targetLang} {sourceLang} {text} 占位符。
		// 如果模板里含 {text}，说明用户想把正文放进 system，
		// 此时不再追加 user 消息里的正文（避免重复）。
		system = expandTemplate(tmpl, targetName, sourceName, r.Source)
	case system != "":
		system = expandTemplate(system, targetName, sourceName, "")
	default:
		system = expandTemplate(defaultSystemPrompt, targetName, sourceName, "")
	}

	msgs := []chatMessage{{Role: "system", Content: system}}
	// 源语言不是 auto 时，在 user 消息里显式声明，能提升小模型的准确率
	if r.SourceLang != lang.Auto && r.SourceLang != "" {
		msgs = append(msgs, chatMessage{
			Role:    "user",
			Content: fmt.Sprintf("源语言：%s\n\n%s", sourceName, r.Source),
		})
	} else {
		msgs = append(msgs, chatMessage{Role: "user", Content: r.Source})
	}
	return msgs
}

// expandTemplate 替换模板占位符。
//
// 用 strings.Replacer 而非 fmt.Sprintf：占位符是命名式的（{targetLang}），
// 而且用户模板里可能出现任意 % 字符（例如翻译含百分号的文本），
// 用 Sprintf 会触发 "unknown verb" 错误。
func expandTemplate(tmpl, targetLang, sourceLang, text string) string {
	r := strings.NewReplacer(
		"{targetLang}", targetLang,
		"{sourceLang}", sourceLang,
		"{text}", text,
	)
	return r.Replace(tmpl)
}

// parseExtraHeaders 解析「额外请求头」配置。
//
// 格式：每行一个 `Name: Value`。用于某些服务商要求的自定义头
// （例如 Azure 的 api-key、某些网关的租户标识）。
//
// 【安全约束】：跳过 Authorization 与 Content-Type ——
// 让用户通过额外头覆盖鉴权头会导致「配了 Key 却用了另一个 Key」的诡异问题，
// 而覆盖 Content-Type 会让请求体解析失败。
func parseExtraHeaders(raw string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.IndexByte(line, ':')
		if i <= 0 {
			continue
		}
		name := strings.TrimSpace(line[:i])
		value := strings.TrimSpace(line[i+1:])
		if name == "" || value == "" {
			continue
		}
		lower := strings.ToLower(name)
		if lower == "authorization" || lower == "content-type" || lower == "content-length" {
			continue
		}
		out[name] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// newRequest 构造带鉴权头的 HTTP 请求。
func (e *engineImpl) newRequest(ctx context.Context, body chatRequest) (*http.Request, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, engine.Wrap(engine.KindConfig, "构造请求失败", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.Endpoint, bytes.NewReader(buf))
	if err != nil {
		return nil, engine.Wrap(engine.KindConfig, "构造请求失败", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}
	// 流式请求必须声明接受 text/event-stream，否则某些网关会缓冲整个响应
	if body.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	for k, v := range e.cfg.ExtraHeaders {
		req.Header.Set(k, v)
	}
	return req, nil
}

// ---------------------------------------------------------------------------
// 非流式翻译
// ---------------------------------------------------------------------------

// chatResponse 是非流式响应体。
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *apiError `json:"error"`
}

// apiError 是服务商返回的错误结构。
//
// 几乎所有 OpenAI 兼容服务商都用这个形状，但字段可选性不一，
// 因此全部用指针/omitempty 处理，缺字段时不报错。
type apiError struct {
	Message string      `json:"message"`
	Type    string      `json:"type"`
	Code    interface{} `json:"code"`
}

func (e *engineImpl) Translate(ctx context.Context, r engine.Request) (engine.Result, error) {
	if err := validateRequest(r); err != nil {
		return engine.Result{}, err
	}
	start := time.Now()

	body := chatRequest{
		Model:       e.cfg.Model,
		Messages:    e.buildMessages(r),
		Stream:      false,
		Temperature: e.cfg.Temperature,
	}
	req, err := e.newRequest(ctx, body)
	if err != nil {
		return engine.Result{}, err
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return engine.Result{}, classifyTransportErr(err, e.cfg.Endpoint)
	}
	defer func() { _ = resp.Body.Close() }()

	// 限制读取量：防止上游返回巨大响应撑爆内存
	raw, err := io.ReadAll(io.LimitReader(resp.Body, engine.MaxResponseBody()))
	if err != nil {
		return engine.Result{}, engine.Wrap(engine.KindNetwork, "读取响应失败", err)
	}

	if resp.StatusCode != http.StatusOK {
		return engine.Result{}, classifyAPIError(resp.StatusCode, raw)
	}

	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return engine.Result{}, engine.Wrap(engine.KindProtocol,
			"响应不是合法的 JSON", err).WithDetail("片段：%s", snippet(raw))
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return engine.Result{}, engine.Newf(engine.KindUpstream,
			"服务端返回错误：%s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return engine.Result{}, engine.New(engine.KindProtocol,
			"响应中没有 choices（可能是模型名不存在或配额已用尽）").
			WithDetail("片段：%s", snippet(raw))
	}

	target := cleanTranslation(parsed.Choices[0].Message.Content)
	if target == "" {
		return engine.Result{}, engine.New(engine.KindProtocol,
			"模型返回了空译文").WithDetail("finish_reason=%s", parsed.Choices[0].FinishReason)
	}
	// finish_reason=length 表示被 max_tokens 截断，译文不完整
	if parsed.Choices[0].FinishReason == "length" {
		return engine.Result{}, engine.New(engine.KindConfig,
			"译文被截断（模型输出长度上限）：请缩短原文或调低「单次请求字符上限」")
	}

	return engine.Result{
		Target:    target,
		ElapsedMs: engine.ElapsedMs(start),
		Chunks:    1,
		Engine:    e.displayName(),
		Model:     e.cfg.Model,
	}, nil
}

func (e *engineImpl) displayName() string {
	if e.name != "" {
		return e.name
	}
	return e.cfg.Model
}

// ---------------------------------------------------------------------------
// 流式翻译
// ---------------------------------------------------------------------------

// streamChunk 是 SSE 增量块。
//
// 不同服务商的字段位置略有差异：
//   - OpenAI/DeepSeek：choices[0].delta.content
//   - 某些兼容实现：choices[0].message.content（即使在流式里）
//   - 通义千问的兼容模式：choices[0].delta.content，但最后一个块带完整 message
//
// 因此两个字段都尝试读取，取非空的那个。
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *apiError `json:"error"`
}

func (e *engineImpl) Stream(ctx context.Context, r engine.Request, emit func(engine.Chunk) error) (engine.Result, error) {
	if err := validateRequest(r); err != nil {
		return engine.Result{}, err
	}
	start := time.Now()

	body := chatRequest{
		Model:       e.cfg.Model,
		Messages:    e.buildMessages(r),
		Stream:      true,
		Temperature: e.cfg.Temperature,
	}
	req, err := e.newRequest(ctx, body)
	if err != nil {
		return engine.Result{}, err
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return engine.Result{}, classifyTransportErr(err, e.cfg.Endpoint)
	}
	// 注意：不能在函数返回前 Close body，否则流式读取会中断。
	// 用 defer 保证任何退出路径都关闭。
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return engine.Result{}, classifyAPIError(resp.StatusCode, raw)
	}

	var (
		builder      strings.Builder
		index        int
		finishReason string
		sawAnyChunk  bool
	)

	scanner := newSSEScanner(resp.Body)
	for scanner.Scan() {
		// 每次循环都检查取消：这是「用户点取消后立即停止读上游」的关键。
		// 不检查的话，取消只会在下一次网络读写时偶然生效，
		// 用户会看到译文继续往外冒好几秒。
		if err := ctx.Err(); err != nil {
			return engine.Result{}, engine.Wrap(engine.KindCancelled, "翻译已取消", err)
		}

		data, ok := scanner.Data()
		if !ok {
			continue // 心跳注释行或空事件
		}
		if data == "[DONE]" {
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// 单个块解析失败不应中断整个流：某些服务商会在流里插入
			// 非 JSON 的调试信息。记录后跳过，继续读下一个块。
			continue
		}
		if chunk.Error != nil && chunk.Error.Message != "" {
			return engine.Result{}, engine.Newf(engine.KindUpstream,
				"服务端返回错误：%s", chunk.Error.Message)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		c := chunk.Choices[0]
		text := c.Delta.Content
		if text == "" {
			text = c.Message.Content
		}
		if c.FinishReason != "" {
			finishReason = c.FinishReason
		}
		if text == "" {
			continue
		}
		sawAnyChunk = true
		builder.WriteString(text)
		if err := emit(engine.Chunk{Text: text, Index: index}); err != nil {
			// emit 失败通常是取消传播：必须立即停止，
			// 否则会白白消耗上游配额。
			return engine.Result{}, engine.Wrap(engine.KindCancelled, "翻译已取消", err)
		}
		index++
	}
	if err := scanner.Err(); err != nil {
		return engine.Result{}, engine.Wrap(engine.KindProtocol, "读取流式响应失败", err)
	}

	if !sawAnyChunk {
		return engine.Result{}, engine.New(engine.KindProtocol,
			"流式响应没有返回任何内容（可能是模型名不存在）").
			WithDetail("finish_reason=%s", finishReason)
	}
	if finishReason == "length" {
		return engine.Result{}, engine.New(engine.KindConfig,
			"译文被截断（模型输出长度上限）：请缩短原文或调低「单次请求字符上限」")
	}

	return engine.Result{
		Target:    cleanTranslation(builder.String()),
		ElapsedMs: engine.ElapsedMs(start),
		Chunks:    index,
		Engine:    e.displayName(),
		Model:     e.cfg.Model,
	}, nil
}

// ---------------------------------------------------------------------------
// SSE 解析
// ---------------------------------------------------------------------------

// sseScanner 按 SSE 规范解析事件流。
//
// SSE 的事件以【空行】分隔，一个事件可以有多行 `data:`，
// 多行的内容需要用 \n 连接后再作为整体解析。
//
// 直接用 bufio.Scanner 逐行读然后立刻解析是【错的】：
// 多行 data 会被拆成多个独立 JSON，每个都解析失败。
type sseScanner struct {
	scanner  *bufio.Scanner
	buffered []string // 当前事件累积的 data 行
	current  string   // 已完成的当前事件数据
	hasEvent bool
	err      error
}

func newSSEScanner(r io.Reader) *sseScanner {
	sc := bufio.NewScanner(r)
	// 默认 64KB 的行缓冲对某些服务商的单行大 JSON 不够
	// （例如带 logprobs 的响应），放大到 1MB。
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return &sseScanner{scanner: sc}
}

// Scan 推进到下一个完整事件。
//
// 返回 false 表示流结束或出错（用 Err() 区分）。
func (s *sseScanner) Scan() bool {
	s.current = ""
	s.hasEvent = false
	s.buffered = s.buffered[:0]

	for s.scanner.Scan() {
		line := s.scanner.Text()

		// 空行 = 事件结束
		if line == "" {
			if len(s.buffered) > 0 {
				s.current = strings.Join(s.buffered, "\n")
				s.hasEvent = true
				return true
			}
			continue // 连续空行，忽略
		}
		// 以 : 开头的是注释行（心跳），必须忽略。
		// 不忽略的话会被当成 data 解析，产生大量无意义的解析失败。
		if strings.HasPrefix(line, ":") {
			continue
		}
		// 解析字段名与值
		field, value := parseSSELine(line)
		switch field {
		case "data":
			s.buffered = append(s.buffered, value)
		case "event", "id", "retry":
			// 这些字段对翻译没有意义，忽略
		}
	}
	// 流结束：如果还有未 flush 的缓冲数据，作为最后一个事件返回。
	// 某些服务商不发结尾空行就直接关闭连接。
	if len(s.buffered) > 0 {
		s.current = strings.Join(s.buffered, "\n")
		s.hasEvent = true
		s.buffered = s.buffered[:0]
		return true
	}
	s.err = s.scanner.Err()
	return false
}

// parseSSELine 拆分 `field: value`。
//
// 规范要求：冒号后的【第一个】空格属于分隔符，要去掉；
// 之后的空格是值的一部分，必须保留。
// 译文里的空格很常见，多去一个就会让单词粘连。
func parseSSELine(line string) (field, value string) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return line, ""
	}
	field = line[:i]
	value = line[i+1:]
	value = strings.TrimPrefix(value, " ")
	return field, value
}

// Data 返回当前事件的数据载荷。
// ok=false 表示当前事件没有 data（例如纯心跳）。
func (s *sseScanner) Data() (string, bool) {
	return s.current, s.hasEvent
}

// Err 返回扫描过程中的错误。
//
// io.EOF 不算错误（正常结束），返回 nil。
func (s *sseScanner) Err() error {
	if s.err == nil || s.err == io.EOF {
		return nil
	}
	return s.err
}

// ---------------------------------------------------------------------------
// 连通性探测
// ---------------------------------------------------------------------------

func (e *engineImpl) Test(ctx context.Context) error {
	// 探测用非流式：更简单，且能一次性验证「端点+Key+模型」三件事。
	// 流式探测需要额外处理 SSE 解析，而探测失败的原因通常与流式无关。
	_, err := e.Translate(ctx, engine.Request{
		Source:     testProbeText,
		SourceLang: "en",
		TargetLang: "zh",
	})
	return err
}

// ---------------------------------------------------------------------------
// 错误分类与结果清洗
// ---------------------------------------------------------------------------

// classifyTransportErr 把网络层错误归类。
//
// 区分几类常见情况，因为它们的用户处理建议完全不同：
//   - DNS 解析失败 → 端点地址写错了
//   - 连接被拒 → 服务没启动或端口错了
//   - TLS 失败 → 证书问题（提示可以开启跳过校验）
//   - 超时 → 网络慢或服务端慢
func classifyTransportErr(err error, endpoint string) error {
	if err == nil {
		return nil
	}
	// 取消判定：http.Client 在 ctx 取消时返回的 *url.Error 会包装
	// context.Canceled，用 errors.Is 沿 Unwrap 链查找即可。
	// （context.Cause 接受的是 context 而不是 error，不能用在这里。）
	if errors.Is(err, context.Canceled) {
		return engine.Wrap(engine.KindCancelled, "翻译已取消", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return engine.Wrap(engine.KindNetwork, "请求超时", err).
			WithDetail("端点：%s", endpoint)
	}
	if errors.Is(err, context.Canceled) {
		return engine.Wrap(engine.KindCancelled, "翻译已取消", err)
	}

	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "no such host"), strings.Contains(lower, "server misbehaving"):
		return engine.Wrap(engine.KindNetwork,
			"域名解析失败：请检查接口地址是否正确", err).WithDetail("端点：%s", endpoint)
	case strings.Contains(lower, "connection refused"):
		return engine.Wrap(engine.KindNetwork,
			"连接被拒绝：服务可能未启动，或端口不正确", err).WithDetail("端点：%s", endpoint)
	case strings.Contains(lower, "x509"), strings.Contains(lower, "certificate"),
		strings.Contains(lower, "tls:"):
		return engine.Wrap(engine.KindConfig,
			"TLS 证书校验失败：证书可能已过期或为自签名。内网端点可开启「跳过 TLS 证书校验」", err).
			WithDetail("端点：%s", endpoint)
	case strings.Contains(lower, "timeout"), strings.Contains(lower, "deadline"):
		return engine.Wrap(engine.KindNetwork, "请求超时", err).
			WithDetail("端点：%s", endpoint)
	case strings.Contains(lower, "proxy"):
		return engine.Wrap(engine.KindNetwork, "代理连接失败：请检查系统代理设置", err)
	case strings.Contains(lower, "connection reset"), strings.Contains(lower, "broken pipe"),
		strings.Contains(lower, "eof"):
		return engine.Wrap(engine.KindNetwork, "连接被中断：请重试", err).
			WithDetail("端点：%s", endpoint)
	}
	return engine.Wrap(engine.KindNetwork, "网络请求失败", err).
		WithDetail("端点：%s", endpoint)
}

// classifyAPIError 把 HTTP 错误状态码归类，并尽量提取服务商的错误消息。
func classifyAPIError(status int, body []byte) error {
	kind, isErr := engine.ClassifyHTTPStatus(status)
	if !isErr {
		kind = engine.KindUpstream
	}
	// 尝试从响应体里提取服务商的错误说明，它通常比状态码更有用
	// （例如 401 的 body 里会写「余额不足」而不是「Key 无效」）
	detail := extractAPIMessage(body)
	base := engine.StatusMessage(status)
	if detail != "" {
		base = base + "：" + detail
	}
	e := engine.New(kind, base)
	// 余额不足是 402/403 的常见原因，单独提示
	if strings.Contains(detail, "余额") || strings.Contains(strings.ToLower(detail), "insufficient") ||
		strings.Contains(strings.ToLower(detail), "quota") || strings.Contains(strings.ToLower(detail), "balance") {
		e.Kind = engine.KindRateLimit
	}
	return e.WithDetail("片段：%s", snippet(body))
}

// extractAPIMessage 从错误响应体里提取人类可读的消息。
//
// 兼容三种常见形状：
//   - {"error":{"message":"..."}}          OpenAI 风格
//   - {"message":"..."}                   简化风格
//   - {"detail":"..."}                    FastAPI 风格
//
// 提取失败时返回空串（调用方会退回用状态码说明）。
// 【绝不】返回整个 body —— 那可能是几 KB 的 HTML 错误页。
func extractAPIMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	// 如果响应是 HTML（服务商网关错误页），直接放弃提取
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && (trimmed[0] == '<') {
		return ""
	}
	var probe struct {
		Error struct {
			Message string      `json:"message"`
			Code    interface{} `json:"code"`
		} `json:"error"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return ""
	}
	for _, candidate := range []string{probe.Error.Message, probe.Message, detailString(probe.Detail)} {
		if s := strings.TrimSpace(candidate); s != "" {
			return truncateRunes(s, 200)
		}
	}
	return ""
}

// detailString 处理 FastAPI 的 detail 字段（可能是字符串或数组）。
func detailString(s string) string { return s }

// cleanTranslation 清洗模型输出，剥离常见的包裹与前后缀。
//
// 即使系统提示词明确要求「只输出译文」，模型仍经常：
//   - 把译文包在引号里："你好"
//   - 加前缀：译文：你好 / Translation: Hello
//   - 用代码块包裹：```\n你好\n```
//   - 尾部加解释：你好（注：这里翻译为...）
//
// 这些都会让译文不可用，必须后处理。
func cleanTranslation(s string) string {
	out := strings.TrimSpace(s)
	if out == "" {
		return ""
	}
	// 剥离整体包裹的代码块围栏
	if strings.HasPrefix(out, "```") {
		// 去掉首行（```lang）与末行（```）
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
		"Translation:", "translation:", "Translated text:", "Here is the translation:",
		"Here is the translation",
	} {
		if strings.HasPrefix(out, prefix) {
			out = strings.TrimSpace(out[len(prefix):])
			break
		}
	}
	// 剥离成对的包裹引号（只在首尾都是引号时才剥，避免误伤正文里的引号）
	out = stripWrappingQuotes(out)
	return strings.TrimSpace(out)
}

// stripWrappingQuotes 剥离首尾成对的引号。
//
// 必须成对才剥：如果只剥开头的引号，会把「"Hello" 是一个词」
// 变成 `Hello" 是一个词`，破坏正文。
func stripWrappingQuotes(s string) string {
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

// validateRequest 校验请求参数。
func validateRequest(r engine.Request) error {
	if strings.TrimSpace(r.Source) == "" {
		return engine.New(engine.KindConfig, "待翻译文本不能为空")
	}
	if strings.TrimSpace(r.TargetLang) == "" {
		return engine.New(engine.KindConfig, "目标语言不能为空")
	}
	if r.TargetLang == lang.Auto {
		return engine.New(engine.KindConfig, "目标语言不能是「自动检测」")
	}
	if !lang.IsValid(r.TargetLang) {
		return engine.Newf(engine.KindConfig, "未知的目标语言码：%s", r.TargetLang)
	}
	if r.SourceLang != "" && !lang.IsValid(r.SourceLang) {
		return engine.Newf(engine.KindConfig, "未知的源语言码：%s", r.SourceLang)
	}
	return nil
}

// snippet 返回响应体片段用于诊断，截断到 300 字符。
func snippet(body []byte) string {
	return truncateRunes(strings.TrimSpace(string(body)), 300)
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// configFloat 从 Config 里安全地取浮点值并夹到范围。
func configFloat(cfg engine.Config, key string, def, lo, hi float64) float64 {
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
