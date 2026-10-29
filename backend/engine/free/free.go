// Package free 实现内置的「免费翻译服务」引擎（哔哩哔哩 Index-Translate）。
//
// # 为什么值得内置
//
// 本插件此前最大的体验缺口是「装完必须先配好引擎才能用」：
// 六个官方引擎都要申请凭据，OpenAI 兼容引擎要填端点和 Key，
// 本地 CLI 引擎要求机器上恰好装了 claude/codex。新用户装好插件打开工作台，
// 看到的是「没有可用引擎」。
//
// 2026-10-26 的实测调研（覆盖 88 个免密钥端点）给出的结论是：
// Google 系（含一切基于它的 Go 库）在国内是【TCP 层阻断】，换 DoH、改 hosts、
// 换镜像域名都无效；LibreTranslate / DeepLX / Lingva 的公共实例 2026 年已全面失效；
// 微软 Edge/Bing 需要完整浏览器指纹；有道/百度的免密钥接口签名已变更。
// 唯一同时满足「免费 + 零配置 + 国内直连 + 质量可用 + 多语言」的，
// 是哔哩哔哩自研并公开部署的 Index-Translate —— 实测延迟 145~950ms、
// 并发 100 全部成功、30 秒内 206 次请求零失败、20+ 语种全通过。
//
// 所以把它做成【安装即存在】的内置引擎，用户装好插件第一次打开就能翻译。
//
// # 为什么复用 openai 适配器，而不是复制一份
//
// 该服务端走的就是 OpenAI Chat Completions 协议。SSE 增量解析、
// 「只输出译文」的提示词模板、译文清洗（剥引号/「译文：」前缀/代码块围栏）、
// HTTP 状态码到错误类别的映射 —— 这些在 openai 包里都已经调通并测过。
// 复制一份等于把八个引擎共有的那套协议逻辑再维护一遍，
// 而「两份实现逐渐漂移」正是本项目反复踩过的坑（见 AGENTS.md 6.8）。
//
// 因此本包只做三件事：注入锁定配置、把 kind 改回 "free"、
// 把「服务端 30 秒网关超时」这个私有知识翻译成人能照做的建议。
package free

import (
	"context"
	"strconv"
	"strings"

	"github.com/vyrnax/translation/engine"
	"github.com/vyrnax/translation/engine/openai"
	"github.com/vyrnax/translation/lang"
)

const (
	// Kind 是引擎类型标识。
	Kind = "free"

	// NameZh / NameEn 是展示名。存储记录里存中文名，
	// 前端对内置引擎优先用 schema 的 label/labelEn，从而随界面语言切换。
	NameZh = "免费翻译服务"
	NameEn = "Free Translation"

	// Endpoint 是 Chat Completions 完整地址。
	// 写成完整路径而不是主机名：openai 适配器虽然会自动补全，
	// 但内置配置不该依赖补全逻辑 —— 显式写死少一个可能出错的地方。
	Endpoint = "https://index-translate.bilibili.com/v1/chat/completions"

	// Model 选 2B 而非 9B：内置引擎的首要任务是「即时翻译不卡」。
	// 实测 2B 平均 245ms、9B 平均 479ms，而 2B 的安全单段上限（400 字符）
	// 也明显小于 9B（800 字符）—— 小上限换来的是更少的 30 秒网关超时。
	Model = "Index-Translate-2B"

	// SafeMaxChars 是单段字符上限，来自实测：2B 在 410 字符时 1136ms 成功，
	// 820 字符时【HTTP 504（30061ms）】。400 是官方文档给出的安全默认值。
	//
	// 不要为了「减少请求数」而调大它：一旦触发 504，整段失败，
	// 重试同样会失败，反而更慢。
	SafeMaxChars = 400

	// TimeoutSecs 必须小于服务端的 30 秒网关硬超时。
	// 20 秒留出余量：让「本地超时」与「服务端 504」都能各自被正确分类，
	// 而不是所有慢请求都糊成同一个错误。
	TimeoutSecs = 20

	// Concurrency 是分段的并发度。实测该端点无 QPS 限制（并发 100 全成功），
	// 而官方文档的实测结论是「8 段并发 351ms vs 串行 2285ms」，提速约 6.5 倍。
	// 取 4 而不是更高：全局在飞任务上限是 4，再叠更高并发只会加剧任务间争抢。
	Concurrency = 4
)

// maxCharsText 是 Description 里引用的上限数字。
//
// 用 strconv 从常量派生，而不是在文案里再手写一个 "400"：
// 调大 SafeMaxChars 时文案会跟着变，不会出现「代码说 800、界面说 400」。
var maxCharsText = strconv.Itoa(SafeMaxChars)

func init() {
	engine.Register(Kind, build, schema())
}

// SeedConfig 返回内置引擎【在存储层里】的配置快照。
//
// 为什么库里要存一份配置：runChunked 的并发数与超时读的是 store.Engine.Config
// 而不是适配器内部（p.semaphore(e.Kind(), ConfigInt(rec.Config, "concurrency", 1, 1, 4))），
// 所以 concurrency/timeoutSecs 必须落在记录里。否则它会退回默认值 ——
// 1 并发 + 全局 60 秒超时，而该服务网关 30 秒就断开，60 秒毫无意义。
//
// noAuth 是 openai 适配器的【内部】开关（它的表单里没有这个字段，用户填不了）：
// 这个端点不需要任何凭据，但它不是 localhost，没有这个出口就只能塞一个假 Key ——
// 而假 Key 会被当作 Authorization 头发出去，反而可能被服务端拒绝。
func SeedConfig() map[string]any {
	return map[string]any{
		"endpoint":    Endpoint,
		"model":       Model,
		"maxChars":    SafeMaxChars,
		"timeoutSecs": TimeoutSecs,
		// 0 而非 openai 默认的 0.3：实测 temperature=0 时同一输入连续 5 次输出
		// 完全一致，且该服务是「翻译模型」而非通用对话模型，不需要流畅度换度。
		"temperature": 0,
		"concurrency": Concurrency,
		"noAuth":      true,
	}
}

// build 是注册到引擎工厂的构造函数。
//
// 它【故意忽略】调用方传进来的 cfg 与 sec：内置引擎不可配置，
// 就算有人绕过 UI 直接调 engines/save 塞进 endpoint，这里也不会采纳。
// 真正的第一道防线在 store 层（内置记录不可改写），
// 这里是第二道 —— 两道都留着，因为「只在一处拦」迟早漏一条路径。
func build(_ engine.Config, _ engine.Secrets, opts engine.Options) (engine.Engine, error) {
	inner, err := openai.New(engine.Config(SeedConfig()), engine.Secrets{}, opts)
	if err != nil {
		// 内置配置全是代码常量，构造失败只可能是代码 bug。
		// 必须响亮失败（而不是退化成「让用户自己填」），CI 与 _verify.mjs 才能立刻发现。
		return nil, engine.Wrap(engine.KindConfig,
			"内置免费引擎初始化失败（这是插件自身的缺陷，请提交 Issue 并附上诊断报告）", err)
	}
	return &engineImpl{Engine: inner}, nil
}

// engineImpl 包住 openai 适配器，只改「身份」和「错误文案」。
type engineImpl struct {
	engine.Engine
}

// Kind 必须改回 "free"。
//
// 为什么这件事不能省：内层适配器返回 "openai"，而 kind 同时是
// 语言映射表索引（lang.SupportedLangs(kind)）与并发信号量的 key
// （p.semaphore(e.Kind(), …）。不覆盖的话，内置引擎会和用户的
// OpenAI 兼容引擎抢同一个信号量，还会拿错语言支持集。
func (e *engineImpl) Kind() string { return Kind }

// SupportedLangs 按本引擎的 kind 取，理由同 Kind()。
// 语言映射内容与 openai 一致（都是大模型自然语言名），但必须走 free 这一份。
func (e *engineImpl) SupportedLangs() []string { return lang.SupportedLangs(Kind) }

// Test 做连通性探测。
func (e *engineImpl) Test(ctx context.Context) error {
	return friendlyError(e.Engine.Test(ctx))
}

// Translate 执行一次性翻译。
func (e *engineImpl) Translate(ctx context.Context, r engine.Request) (engine.Result, error) {
	res, err := e.Engine.Translate(ctx, r)
	return res, friendlyError(err)
}

// Stream 执行流式翻译。
func (e *engineImpl) Stream(ctx context.Context, r engine.Request, emit func(engine.Chunk) error) (engine.Result, error) {
	res, err := e.Engine.Stream(ctx, r, emit)
	return res, friendlyError(err)
}

// friendlyError 把「服务端 30 秒网关超时」翻译成人能照做的建议。
//
// 实测：该服务网关 30 秒硬超时，超时返回 HTTP 504 + HTML 错误页（不是 JSON），
// 通用分类只会给出「上游服务异常（504）」。用户看到这个，既不知道该缩短文本，
// 也不知道该换引擎 —— 而这恰恰是唯一两个有效动作。
//
// 只改这类【我们确切知道原因】的错误，其余错误原样透传：
// 自作聪明地改写所有错误信息，会把服务商返回的真实原因（配额、模型下线等）
// 一起盖掉，那是更难排查的问题。
func friendlyError(err error) error {
	if err == nil {
		return nil
	}
	oe := engine.Classify(err)
	if oe == nil {
		return err
	}
	if oe.Kind != engine.KindUpstream || !contains504(oe.Message, oe.Detail) {
		return err
	}
	oe.Message = "免费翻译服务在 30 秒内没有返回结果（服务端网关超时）。" +
		"原因通常是单段文本过长或该公益端点正繁忙；直接重试同一内容多半还会失败。"
	oe.Detail = "单段安全上限 " + maxCharsText + " 字符。建议：缩短文本、稍后再试，" +
		"或在引擎管理中配置一个官方引擎（DeepL / 百度 / 有道 / 阿里云 / 腾讯云 / Google / 自有大模型）作为主力。"
	return oe
}

// contains504 判断错误是否指向 HTTP 504 网关超时。
//
// 要求「同时出现 504 与网关/超时语境词」而不是只匹配 "504"：
// 上游错误详情里可能出现别的数字（token 数、请求 ID、时间戳），
// 只匹配数字会把无关错误改写成超时建议 —— 那是比原样透传更糟的误导。
func contains504(msg, detail string) bool {
	if !strings.Contains(msg, "504") && !strings.Contains(detail, "504") {
		return false
	}
	blob := strings.ToLower(msg + " " + detail)
	return strings.Contains(blob, "gateway") ||
		strings.Contains(blob, "timeout") ||
		strings.Contains(msg, "超时")
}
