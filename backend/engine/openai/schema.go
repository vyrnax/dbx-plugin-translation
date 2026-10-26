package openai

import "github.com/gaogao/translation/engine"

// schema 返回本引擎的配置表单描述。
//
// 这份描述由 translate/schema 下发给前端，前端的通用渲染器据此生成表单。
// 【新增引擎时前端零改动】的架构承诺就依赖这里 —— 字段定义与使用它的
// 适配器代码在同一个包里，不会出现「后端加了字段但前端表单没跟上」。
func schema() engine.Schema {
	return engine.Schema{
		Kind:    Kind,
		Label:   "OpenAI 兼容大模型",
		LabelEn: "OpenAI-compatible LLM",
		// 默认名用「大模型」而不是「OpenAI」：这套协议被 DeepSeek、通义、
		// 智谱、Kimi、Ollama、vLLM 等大量服务复用，写死 OpenAI 会误导用户。
		DefaultName:   "大模型翻译",
		DefaultNameEn: "LLM Translation",
		Description:   "适用于 OpenAI、DeepSeek、通义千问、智谱、Kimi/Moonshot、Ollama、vLLM、LM Studio 等所有实现了 /v1/chat/completions 协议的服务。支持流式输出。",
		DescriptionEn: "Works with any service implementing /v1/chat/completions: OpenAI, DeepSeek, Qwen, Zhipu, Kimi/Moonshot, Ollama, vLLM, LM Studio. Supports streaming.",
		Streaming:     true,
		MaxChars:      defaultMaxChars,
		NeedsSecret:   true,
		SecretLabel:   "API Key",
		Fields: []engine.Field{
			{
				Key:           "endpoint",
				Label:         "接口地址",
				LabelEn:       "API endpoint",
				Type:          engine.FieldText,
				Required:      true,
				Placeholder:   "https://api.deepseek.com/v1/chat/completions",
				Description:   "完整的 Chat Completions 地址。可只填到主机名或 /v1，会自动补全为 /v1/chat/completions。本地服务可填 http://127.0.0.1:11434/v1/chat/completions",
				DescriptionEn: "Full Chat Completions URL. Host-only or /v1 forms are auto-completed to /v1/chat/completions. For local servers use http://127.0.0.1:11434/v1/chat/completions",
			},
			{
				Key:           "model",
				Label:         "模型名称",
				LabelEn:       "Model name",
				Type:          engine.FieldText,
				Required:      true,
				Placeholder:   "deepseek-chat / gpt-4o-mini / qwen-turbo",
				Description:   "服务商提供的模型标识符，必须与服务商文档完全一致。",
				DescriptionEn: "Model identifier exactly as documented by the provider.",
			},
			{
				Key:           "apiKey",
				Label:         "API Key",
				LabelEn:       "API Key",
				Type:          engine.FieldPassword,
				Secret:        true,
				Required:      true,
				Placeholder:   "sk-...",
				Description:   "由系统密钥库加密保存，界面只显示掩码指纹。本地服务（Ollama 等）可留空。",
				DescriptionEn: "Encrypted with the OS keychain; only a masked fingerprint is shown. May be left empty for local servers such as Ollama.",
			},
			{
				Key:           "temperature",
				Label:         "采样温度",
				LabelEn:       "Temperature",
				Type:          engine.FieldNumber,
				Default:       defaultTemperature,
				Min:           engine.NumPtr(0),
				Max:           engine.NumPtr(2),
				Description:   "越低越忠实原文，越高越有文采。翻译场景推荐 0.1–0.5。",
				DescriptionEn: "Lower is more faithful, higher is more creative. 0.1-0.5 is recommended for translation.",
			},
			engine.MaxCharsField(defaultMaxChars),
			engine.TimeoutField(engine.DefaultTimeoutSecs),
			engine.ConcurrencyField(1,
				"同时发往该引擎的最大请求数。分段翻译长文时提高此值可加速，但可能触发服务商限流。",
				"Maximum concurrent requests to this engine. Raising it speeds up chunked translation but may trigger rate limits."),
			{
				Key:           "systemPrompt",
				Label:         "自定义系统提示词",
				LabelEn:       "Custom system prompt",
				Type:          engine.FieldTextarea,
				Multiline:     5,
				Placeholder:   "留空使用内置翻译模板。可用占位符：{targetLang} {sourceLang}",
				Description:   "留空时使用经过调优的内置模板（强调「只输出译文」「保留格式与占位符」）。支持占位符：{targetLang} 目标语言、{sourceLang} 源语言。",
				DescriptionEn: "Leave empty to use the tuned built-in template. Placeholders: {targetLang}, {sourceLang}.",
			},
			{
				Key:           "extraHeaders",
				Label:         "额外请求头",
				LabelEn:       "Extra request headers",
				Type:          engine.FieldTextarea,
				Multiline:     3,
				Placeholder:   "X-Tenant-Id: my-org\nX-Custom: value",
				Description:   "每行一个 Name: Value。用于某些网关要求的自定义头。Authorization 与 Content-Type 不可覆盖（会被忽略）。",
				DescriptionEn: "One Name: Value per line, for gateways requiring custom headers. Authorization and Content-Type cannot be overridden.",
			},
			engine.SkipTLSField(),
		},
	}
}
