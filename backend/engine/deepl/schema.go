package deepl

import "github.com/gaogao/translation/engine"

// schema 返回 DeepL 的配置表单描述。
func schema() engine.Schema {
	return engine.Schema{
		Kind:          Kind,
		Label:         "DeepL",
		LabelEn:       "DeepL",
		DefaultName:   "DeepL 翻译",
		DefaultNameEn: "DeepL",
		Description: "DeepL 是翻译质量公认最好的机翻服务之一，尤其擅长欧洲语言互译。" +
			"免费档每月 50 万字符，付费档按量计费。不支持流式输出。",
		DescriptionEn: "DeepL is widely regarded as one of the best machine translation services, " +
			"especially for European languages. Free tier: 500K chars/month. No streaming support.",
		Streaming:   false,
		MaxChars:    maxCharsPerRequest,
		NeedsSecret: true,
		SecretLabel: "API Key",
		Fields: []engine.Field{
			{
				Key:      "serverKind",
				Label:    "服务档位",
				LabelEn:  "Service tier",
				Type:     engine.FieldRadio,
				Required: true,
				Default:  "free",
				Options: []engine.Option{
					{Label: "免费档（api-free.deepl.com）", Value: "free"},
					{Label: "付费档（api.deepl.com）", Value: "pro"},
				},
				Description: "必须与 API Key 匹配：免费档的 Key 以 :fx 结尾。档位与 Key 不匹配会返回 403。" +
					"填了自定义接口地址时此项被忽略。",
				DescriptionEn: "Must match your API key: free-tier keys end with :fx. A mismatch returns 403. " +
					"Ignored when a custom endpoint is set.",
			},
			{
				Key:           "apiKey",
				Label:         "API Key",
				LabelEn:       "API Key",
				Type:          engine.FieldPassword,
				Secret:        true,
				Required:      true,
				Placeholder:   "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx:fx",
				Description:   "在 DeepL 账户设置 → API 密钥 中获取。免费档的 Key 以 :fx 结尾。由系统密钥库加密保存。",
				DescriptionEn: "Get it from DeepL account → API keys. Free-tier keys end with :fx. Encrypted by the OS keychain.",
			},
			{
				Key:           "endpoint",
				Label:         "自定义接口地址",
				LabelEn:       "Custom endpoint",
				Type:          engine.FieldText,
				Placeholder:   "留空使用官方端点",
				Description:   "多数情况留空即可。仅在使用企业代理或自建网关时填写，例如 https://deepl-proxy.internal/v2/translate",
				DescriptionEn: "Leave empty in most cases. Fill only for an enterprise proxy or self-hosted gateway.",
			},
			{
				Key:     "formality",
				Label:   "语体风格",
				LabelEn: "Formality",
				Type:    engine.FieldSelect,
				Default: "default",
				Options: []engine.Option{
					{Label: "默认（由 DeepL 决定）", Value: "default"},
					{Label: "更正式", Value: "prefer_more"},
					{Label: "更口语", Value: "prefer_less"},
				},
				Description:   "仅对德语、法语、意大利语、西班牙语、荷兰语、波兰语、葡萄牙语、俄语、日语生效，其它语言会被忽略。",
				DescriptionEn: "Only applies to German, French, Italian, Spanish, Dutch, Polish, Portuguese, Russian and Japanese.",
			},
			{
				Key:           "glossaryId",
				Label:         "术语表 ID",
				LabelEn:       "Glossary ID",
				Type:          engine.FieldText,
				Placeholder:   "留空不使用术语表",
				Description:   "用于保证专有名词译法一致。注意：使用术语表时必须显式指定源语言（不支持自动检测）。",
				DescriptionEn: "Keeps terminology consistent. Note: using a glossary requires an explicit source language (auto-detect is not supported).",
			},
			engine.TimeoutField(engine.DefaultTimeoutSecs),
			engine.ConcurrencyField(1,
				"同时发往 DeepL 的最大请求数。免费档建议保持 1，避免触发速率限制。",
				"Maximum concurrent requests. Keep at 1 on the free tier to avoid rate limits."),
			engine.SkipTLSField(),
		},
	}
}
