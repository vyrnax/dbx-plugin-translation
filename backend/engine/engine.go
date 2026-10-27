// Package engine 定义翻译引擎的统一抽象与注册表。
//
// # 为什么要抽象成接口
//
// 六类引擎的协议差异极大：OpenAI 兼容是 JSON + SSE 流式、DeepL 是 REST、
// 百度/有道/阿里各有各的签名算法、本地 CLI 是子进程 + 行缓冲输出。
// 如果让 main.go 直接面对这些差异，RPC 层会被 if-else 淹没，
// 每加一个引擎都要改核心分发逻辑。
//
// 抽出 Engine 接口后：
//   - main.go 只依赖接口，不知道具体引擎
//   - 新增引擎 = 新增一个子包 + 一行 Register 调用
//   - 前端表单由 translate/schema 动态渲染，【新增引擎前端零改动】
//
// # 分段与流式的职责划分
//
// 分段（chunk 包）由【调用方】（main.go）负责，不在适配器里做。
// 适配器只处理「一次请求」，收到的一定是已经符合自己上限的文本。
//
// 这样划分的原因：分段策略（段间延时、保序拼接、失败重试）是跨引擎共通的，
// 放进每个适配器会重复六遍；而「单次上限」是引擎私有知识，
// 通过 MaxChars() 暴露给调用方即可。
//
// 同理，流式的「非流式引擎降级为单 chunk」也在这里统一处理：
// SupportsStreaming() 返回 false 时，调用方用 Translate() 的结果
// 发一个 chunk 再发 end，前端代码路径完全一致，不需要分支。
package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Request 是一次翻译请求。
//
// Source 保证非空且长度不超过该引擎的 MaxChars()（由调用方分段后保证）。
// SourceLang / TargetLang 是【规范语言码】（见 lang 包），
// 适配器内部负责映射成引擎私有码 —— 这个映射不允许在适配器里手写，
// 必须走 lang.Map，否则六份映射表会各自漂移。
type Request struct {
	Source     string
	SourceLang string // 规范码，可能是 "auto"
	TargetLang string // 规范码，绝不会是 "auto"（lang.MapTarget 已拦截）

	// MaxChars 是调用方告知的单次上限，适配器可用于二次校验。
	// 0 表示调用方未限制（例如 Test 探测时的短文本）。
	MaxChars int
}

// Chunk 是流式输出的一个片段。
type Chunk struct {
	Text  string // 增量文本（不是累计全文）
	Index int    // 片段序号，从 0 开始
	Total int    // 已知总段数；流式场景下通常未知，填 0
}

// Result 是一次翻译的完整结果。
type Result struct {
	Target       string // 完整译文
	DetectedLang string // 引擎检测出的源语言（规范码，可能为空）
	ElapsedMs    int64  // 耗时（毫秒）
	Chunks       int    // 分段数（未分段时为 1）
	Engine       string // 引擎显示名（用于历史记录的展示）
	Model        string // 模型标识（openai 的 model / deepl 的版本等）
}

// ElapsedMs 返回自 start 起经过的毫秒数，【向上取整】，且保证非负。
//
// # 为什么要向上取整
//
// time.Duration.Milliseconds() 是【向下截断】：0.4ms 会变成 0ms。
// 界面上显示「耗时 0ms」看着像计时功能坏了，而实际上确实花了时间、只是不足 1ms。
// 向上取整保证「可测量的非零耗时」至少报 1ms，语义更诚实。
//
// # 为什么保证非负
//
// 时钟回拨（系统时间被向前调整过）时 time.Since 会得到负的 Duration。
// 不处理的话界面会显示「-3ms」这种荒谬值，所以钳到 0。
//
// # ⚠️ 它【不能】保证返回正数，也不该有人这么假设
//
// 0 是合法返回值，因为 time.Since(start) 本身可能就是 0：
// 实测 Windows 本机连续两次 time.Now() 有 100% 概率返回完全相同的值
// （200 次采样全为 0），200 次进程内 httptest 往返里 177 次 time.Since == 0。
// Linux CI 上则是时钟细但机器快，一次往返常在 0.x ms 内完成。
// 0 向上取整还是 0 —— 这是时钟粒度问题，不是取整方式能解决的。
//
// 本项目 v0.2.2 的 CI 曾因此随机红绿：openai_test.go 断言 `ElapsedMs <= 0` 报错，
// 等于假设「一次翻译至少花 1ms」，而该假设在快机器上不成立。
// **真正的修法是改掉那条断言（只断言非负），而不是把实现硬掰成正数** ——
// 断言必须来自规范，不能来自「实现现在输出什么」（见 AGENTS.md 6.8）。
//
// # 为什么放在 engine 包而不是各处自己算
//
// 8 个引擎适配器 + main.go 的 RPC 层共 14 处都要算耗时。
// 各自写 time.Since(start).Milliseconds() 就会各自带着同一套截断/负数缺陷，
// 改的时候要动 14 个地方、漏一个就留一个坑。收敛成一个函数，
// 缺陷只可能存在一处、修一次就全修好。
//
// 参数 start 是调用方在发起请求前记录的 time.Now()；返回毫秒数（>= 0）。

func ElapsedMs(start time.Time) int64 {
	d := time.Since(start)
	if d <= 0 {
		return 0
	}
	const ms = int64(time.Millisecond)
	return (d.Nanoseconds() + ms - 1) / ms
}

// Engine 是所有翻译引擎适配器的统一接口。
//
// 实现约束：
//   - 所有方法必须尊重 ctx 的取消与超时（取消后要立即停止读上游 body）
//   - 实现必须是并发安全的：同一个引擎实例可能被多个翻译任务同时使用
//   - 错误必须用 Classify 包装成 *Error，让上层能映射到正确的错误码
//   - 【绝不】把密钥写进错误消息或日志
type Engine interface {
	// Kind 返回引擎类型标识，与注册时的 kind 一致。
	Kind() string

	// SupportsStreaming 报告是否支持增量输出。
	// false 时调用方会走「单 chunk 降级」路径。
	SupportsStreaming() bool

	// MaxChars 返回单次请求的字符上限（rune 计数，不是字节）。
	// 调用方据此决定是否需要分段。
	MaxChars() int

	// SupportedLangs 返回支持的规范语言码（含 auto，若支持源语言自动检测）。
	SupportedLangs() []string

	// Test 做连通性与鉴权探测，用最短的文本试译一次。
	//
	// 成功返回 nil。失败必须返回 *Error 且带正确的 Kind，
	// 因为前端会把错误直接展示给用户（「API Key 无效」vs「网络不可达」
	// 是完全不同的处理建议）。
	Test(ctx context.Context) error

	// Translate 执行一次性翻译（非流式）。
	Translate(ctx context.Context, r Request) (Result, error)

	// Stream 执行流式翻译，每收到一个增量就调用 emit。
	//
	// emit 返回错误时必须立即停止并返回该错误 —— 这是取消传播的通道：
	// 前端取消后，调用方的 emit 会返回 context.Canceled，
	// 适配器据此停止读上游 body，避免白白消耗配额。
	//
	// 不支持流式的引擎【不需要】实现真正的流式逻辑：
	// 调用方会先查 SupportsStreaming()，false 时直接走 Translate()。
	// 但为了接口完整性，实现里可以简单地「调 Translate 再一次性 emit」。
	Stream(ctx context.Context, r Request, emit func(Chunk) error) (Result, error)
}

// Config 是引擎的非敏感配置（端点、模型名、超时等）。
//
// 来自 store.Engine.Config，即用户在表单里填的所有【非密钥】字段。
type Config map[string]any

// Secrets 是引擎的敏感凭据。
//
// 用 map 而非固定结构体：不同引擎的凭据字段名不同
// （openai 是 apiKey、百度是 secretKey、有道是 appSecret、阿里是 accessKeySecret），
// 用 map 才能让适配器各取所需，也避免了「结构体里一堆只有一个引擎用的字段」。
type Secrets map[string]string

// Options 是构造引擎时的运行时选项。
type Options struct {
	// Name 是引擎的显示名，会写进 Result.Engine 供历史记录展示。
	Name string

	// TimeoutSecs 是单次请求超时（秒）。0 表示用适配器默认值。
	TimeoutSecs int

	// SkipTLSVerify 关闭 TLS 证书校验。
	//
	// 【危险选项】：仅用于自签名证书的内网 LLM 端点。
	// 开启时 UI 必须显示红色警示，侧车必须在 trace 里记录（但不记录密钥）。
	SkipTLSVerify bool

	// Concurrency 是该引擎的最大并发请求数（信号量容量）。
	// 百度/有道免费档是 1 QPS，默认必须为 1。
	Concurrency int
}

// Factory 构造一个引擎实例。
//
// 每次翻译都新建实例（而不是复用单例）：这样配置变更立即生效，
// 不需要处理「用户改了端点但旧实例还在用旧配置」的问题。
// 构造成本很低（只是填结构体），不涉及网络。
type Factory func(cfg Config, sec Secrets, opts Options) (Engine, error)

// ---------------------------------------------------------------------------
// 错误分类
// ---------------------------------------------------------------------------

// ErrorKind 是错误类别，决定映射到哪个 JSON-RPC 错误码与是否可重试。
type ErrorKind int

const (
	// KindNetwork 是网络层失败：DNS 解析失败、连接被拒、超时、TLS 握手失败。
	// → -32030，用户可手动重试。
	KindNetwork ErrorKind = iota

	// KindAuth 是鉴权失败：401/403、签名错误、Key 无效。
	// → -32031，【绝不自动重试】—— 重试无意义且白白消耗配额，
	// 某些服务商还会因为频繁失败而临时封禁。
	KindAuth

	// KindRateLimit 是限流：429、配额耗尽、QPS 超限。
	// → -32032，提示用户稍后再试。
	KindRateLimit

	// KindProtocol 是响应格式非法：返回的不是 JSON、缺少必需字段、SSE 结构损坏。
	// → -32033，附带截断的原始片段便于定位（服务商改了 API 时最常见）。
	KindProtocol

	// KindCancelled 是用户主动取消。→ -32034，不算失败，UI 不弹错误提示。
	KindCancelled

	// KindConfig 是配置问题：端点为空、必填项缺失、模型名非法。
	// → -32602（参数非法），提示用户去检查引擎配置。
	KindConfig

	// KindUnsupported 是能力不支持：例如要求流式但引擎不支持、
	// 或目标语言该引擎不支持。→ -32601。
	KindUnsupported

	// KindUpstream 是上游服务端错误：5xx、服务商内部异常。
	// → -32030（与网络失败同类处理：可重试）。
	KindUpstream
)

// Error 是引擎层的统一错误类型。
//
// 所有适配器返回的错误都必须是这个类型（或用 Wrap 包装成它），
// 这样 main.go 只需要一次类型断言就能拿到 Kind 与可重试性，
// 不需要对每个引擎的错误消息做字符串匹配（那是最脆弱的做法）。
type Error struct {
	Kind    ErrorKind
	Message string // 面向用户的中文消息，可直接展示
	Detail  string // 技术细节，进日志与诊断报告，【不含密钥】
	Err     error  // 底层错误，供 errors.Is/As 使用
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return e.Message + "（" + e.Detail + "）"
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Err }

// Code 返回对应的 JSON-RPC 错误码。
func (e *Error) Code() int {
	switch e.Kind {
	case KindAuth:
		return -32031
	case KindRateLimit:
		return -32032
	case KindProtocol:
		return -32033
	case KindCancelled:
		return -32034
	case KindConfig:
		return -32602
	case KindUnsupported:
		return -32601
	case KindNetwork, KindUpstream:
		return -32030
	}
	return -32030
}

// Retryable 报告是否值得自动重试。
//
// 只有网络抖动与上游 5xx 可重试。鉴权失败与限流【绝不】自动重试：
// 前者重试必然再失败，后者重试只会加重限流甚至触发封禁。
func (e *Error) Retryable() bool {
	return e.Kind == KindNetwork || e.Kind == KindUpstream
}

// KindName 返回类别的可读名，用于日志与诊断。
func (e *Error) KindName() string {
	switch e.Kind {
	case KindNetwork:
		return "network"
	case KindAuth:
		return "auth"
	case KindRateLimit:
		return "rate-limit"
	case KindProtocol:
		return "protocol"
	case KindCancelled:
		return "cancelled"
	case KindConfig:
		return "config"
	case KindUnsupported:
		return "unsupported"
	case KindUpstream:
		return "upstream"
	}
	return "unknown"
}

// New 构造一个引擎错误。
func New(kind ErrorKind, message string) *Error {
	return &Error{Kind: kind, Message: message}
}

// Newf 构造一个带格式化的引擎错误。
func Newf(kind ErrorKind, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

// Wrap 把底层错误包装成引擎错误，保留 errors.Is/As 链路。
func Wrap(kind ErrorKind, message string, err error) *Error {
	return &Error{Kind: kind, Message: message, Err: err, Detail: describe(err)}
}

// WithDetail 给错误补充技术细节。
func (e *Error) WithDetail(format string, args ...any) *Error {
	e.Detail = fmt.Sprintf(format, args...)
	return e
}

// describe 生成错误的简短描述，截断到 200 字符。
//
// 截断是必要的：上游返回的错误体可能是整个 HTML 错误页（几 KB），
// 全塞进 Detail 会让诊断报告不可读。
func describe(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	const max = 200
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// Classify 把任意错误归类为 *Error。
//
// 这是 main.go 唯一需要调用的错误处理入口：
//   - 已经是 *Error 的直接返回
//   - context.Canceled → KindCancelled
//   - context.DeadlineExceeded → KindNetwork（超时）
//   - 其余 → KindNetwork（保守归类为可重试的网络问题）
//
// 保守归类的理由：未知错误更可能是网络/上游问题而非配置问题，
// 归为可重试让用户有机会再试一次，比直接报「配置错误」误导用户更好。
func Classify(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	if errors.Is(err, context.Canceled) {
		return &Error{Kind: KindCancelled, Message: "翻译已取消", Err: err}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{
			Kind:    KindNetwork,
			Message: "翻译超时",
			Detail:  describe(err),
			Err:     err,
		}
	}
	return &Error{
		Kind:    KindNetwork,
		Message: "翻译失败",
		Detail:  describe(err),
		Err:     err,
	}
}

// IsCancelled 判断错误是否源于取消。
func IsCancelled(err error) bool {
	e := Classify(err)
	return e != nil && e.Kind == KindCancelled
}

// ---------------------------------------------------------------------------
// 注册表
// ---------------------------------------------------------------------------

// registration 是一条引擎注册记录。
type registration struct {
	kind    string
	factory Factory
	schema  Schema
}

var (
	registryMu sync.RWMutex
	registry   = map[string]registration{}
	// registerOrder 记录注册顺序，保证 Kinds() 输出稳定（便于测试断言与 UI 排序）
	registerOrder []string
)

// Register 注册一个引擎类型。
//
// 必须在包的 init() 里调用。重复注册同一 kind 会【覆盖】并记录警告 ——
// 不 panic，因为 panic 会让整个侧车启动失败，而侧车崩溃的表现是
// 「插件完全无响应」，排查成本远高于一个被覆盖的注册。
func Register(kind string, factory Factory, schema Schema) {
	kind = strings.TrimSpace(kind)
	if kind == "" || factory == nil {
		return
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[kind]; !dup {
		registerOrder = append(registerOrder, kind)
	}
	schema.Kind = kind
	registry[kind] = registration{kind: kind, factory: factory, schema: schema}
}

// Lookup 按 kind 取注册记录。
func Lookup(kind string) (Factory, Schema, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	r, ok := registry[strings.TrimSpace(kind)]
	if !ok {
		return nil, Schema{}, false
	}
	return r.factory, r.schema, true
}

// Kinds 返回全部已注册的引擎 kind（按注册顺序）。
func Kinds() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, len(registerOrder))
	copy(out, registerOrder)
	return out
}

// Schemas 返回全部已注册引擎的 schema（按注册顺序），供 translate/schema 下发。
func Schemas() []Schema {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]Schema, 0, len(registry))
	for _, k := range registerOrder {
		if r, ok := registry[k]; ok {
			out = append(out, r.schema)
		}
	}
	return out
}

// Build 构造一个引擎实例。
//
// kind 未注册时返回 KindConfig 错误（而不是 nil, false），
// 因为「用户配了一个不存在的引擎类型」是需要明确告知的配置问题。
func Build(kind string, cfg Config, sec Secrets, opts Options) (Engine, error) {
	factory, _, ok := Lookup(kind)
	if !ok {
		return nil, Newf(KindConfig, "未知的引擎类型：%s", kind)
	}
	if cfg == nil {
		cfg = Config{}
	}
	if sec == nil {
		sec = Secrets{}
	}
	e, err := factory(cfg, sec, opts)
	if err != nil {
		return nil, Classify(err)
	}
	if e == nil {
		return nil, Newf(KindConfig, "引擎 %s 构造失败：返回了空实例", kind)
	}
	return e, nil
}

// ResetRegistryForTest 清空注册表，仅供测试使用。
func ResetRegistryForTest() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = map[string]registration{}
	registerOrder = nil
}

// SortKinds 按字典序排序 kind 列表（测试断言用）。
func SortKinds(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	sort.Strings(out)
	return out
}
