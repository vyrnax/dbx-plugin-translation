package google

import "github.com/gaogao/translation/engine"

// schema 返回 Google 翻译的配置表单描述。
func schema() engine.Schema {
	return engine.Schema{
		Kind:          Kind,
		Label:         "Google 翻译",
		LabelEn:       "Google Translate",
		DefaultName:   "Google 翻译",
		DefaultNameEn: "Google Translate",
		Description: "Google Cloud Translation API (v2)。需在 Google Cloud Console 开通 " +
			"Cloud Translation API 并创建 API Key。前 50 万字符/月免费，超出按量计费。不支持流式输出。",
		DescriptionEn: "Google Cloud Translation API v2. Enable Cloud Translation API in Google Cloud " +
			"Console and create an API key. First 500K chars/month free. No streaming.",
		Streaming:   false,
		MaxChars:    maxCharsPerRequest,
		NeedsSecret: true,
		SecretLabel: "API Key",
		Fields: []engine.Field{
			{
				Key:         "apiKey",
				Label:       "API Key",
				LabelEn:     "API Key",
				Type:        engine.FieldPassword,
				Secret:      true,
				Required:    true,
				Placeholder: "AIza...",
				Description: "在 Google Cloud Console → API 和服务 → 凭据 中创建。需先启用 Cloud Translation API。" +
					"由系统密钥库加密保存。",
				DescriptionEn: "Create in Google Cloud Console → APIs & Services → Credentials. " +
					"Enable Cloud Translation API first. Encrypted by the OS keychain.",
			},
			{
				Key:           "endpoint",
				Label:         "自定义接口地址",
				LabelEn:       "Custom endpoint",
				Type:          engine.FieldText,
				Placeholder:   "留空使用官方端点",
				Description:   "多数情况留空即可。仅在使用反向代理（例如国内访问需要代理）时填写。",
				DescriptionEn: "Leave empty in most cases. Fill only when using a reverse proxy.",
			},
			engine.TimeoutField(engine.DefaultTimeoutSecs),
			engine.ConcurrencyField(1,
				"同时发往 Google 的最大请求数。免费额度有 QPS 限制，建议保持 1。",
				"Maximum concurrent requests. Free quota has QPS limits; keep at 1."),
			engine.SkipTLSField(),
		},
	}
}
