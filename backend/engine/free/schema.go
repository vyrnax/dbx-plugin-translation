package free

import "github.com/vyrnax/translation/engine"

// schema 返回内置免费引擎的表单描述。
//
// # Fields 是空的，这是刻意设计
//
// 内置引擎的定位是「装好就能用」，任何需要用户填的字段都违背这个定位。
// 而前端的表单是由 Fields 渲染的，空数组就自然渲染成「无配置项」——
// 不需要前端为它写任何特例代码。
//
// 端点、模型、单次上限这些事实全部写在 Description 里，
// 供「内置引擎说明」弹框展示。它们是代码常量决定的，
// 不该伪装成可编辑字段：让用户改一个内置引擎的端点，
// 等于把一个公益端点变成「用户自己填的端点」，那和 openai 引擎有什么区别。
func schema() engine.Schema {
	return engine.Schema{

		Kind:    Kind,
		Icon:    "default.svg",
		Label:   NameZh,
		LabelEn: NameEn,
		// DefaultName 只在「作为存储记录的名字」时用（store 里的 Name 字段）。
		// 前端展示内置引擎时会优先用 label/labelEn，从而随界面语言切换。
		DefaultName:   NameZh,
		DefaultNameEn: NameEn,
		Description: "无需任何配置、无需 API Key 的内置引擎。服务端是哔哩哔哩自研并公开部署的 Index-Translate 翻译模型，" +
			"走 OpenAI Chat Completions 协议，国内可直连、无需注册。\n\n" +
			"【为什么它不能当主力引擎】它是公益性的公共享用端点：不保证长期可用，高峰时段可能变慢或失败，" +
			"且服务端网关有 30 秒硬超时（超时返回 HTTP 504），因此单段文本上限被压到 " + maxCharsText + " 字符。\n\n" +
			"【建议用法】把它当作「开箱即用」和「官方引擎全部不可用时的兜底」；" +
			"日常稳定使用请在引擎管理里配置官方引擎（DeepL、百度、有道、阿里云、腾讯云、Google，或你自己的大模型端点）。",
		DescriptionEn: "Built-in engine with zero configuration and no API key. It is backed by Bilibili's publicly deployed " +
			"Index-Translate model over the OpenAI Chat Completions protocol — directly reachable from mainland China, no signup.\n\n" +
			"[Why it should not be your primary engine] This is a shared community endpoint: availability is not guaranteed, " +
			"it can slow down or fail at peak times, and the server enforces a 30-second gateway timeout (HTTP 504), " +
			"so each segment is capped at " + maxCharsText + " characters.\n\n" +
			"[Recommended use] Treat it as an out-of-the-box default and a fallback when every configured engine fails. " +
			"For dependable day-to-day translation, add an official engine (DeepL, Baidu, Youdao, Alibaba Cloud, " +
			"Tencent Cloud, Google, or your own LLM endpoint).",
		Streaming:   true,
		MaxChars:    SafeMaxChars,
		NeedsSecret: false,
		// Builtin：前端据此隐藏「编辑 / 删除」，并从「添加引擎」的类型下拉里排除本类型。
		Builtin: true,
		// NoThrottle：实测该端点无限流（并发 100 全成功、30 秒内 206 次请求零失败），
		// 因此不沿用全局 1100ms 段间延时。
		NoThrottle: true,
		Fields:     []engine.Field{},
	}
}
