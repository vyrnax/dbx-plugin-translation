package youdao

import "github.com/gaogao/translation/engine"

// schema 返回有道翻译的配置表单描述。
func schema() engine.Schema {
	return engine.Schema{
		Kind:          Kind,
		Label:         "有道翻译",
		LabelEn:       "Youdao Translate",
		DefaultName:   "有道翻译",
		DefaultNameEn: "Youdao Translate",
		Description: "有道智云「文本翻译」API v3。需在控制台创建应用并绑定「文本翻译」服务。" +
			"注意：有道不支持自动检测源语言，需在翻译页显式选择。不支持流式输出。",
		DescriptionEn: "Youdao Zhiyun text translation API v3. Create an app in the console and bind the " +
			"text translation service. Auto source-language detection is not supported. No streaming.",
		Streaming:   false,
		MaxChars:    maxCharsPerRequest,
		NeedsSecret: true,
		SecretLabel: "应用密钥（App Secret）",
		Fields: []engine.Field{
			{
				Key:           "appKey",
				Label:         "应用 ID（App Key）",
				LabelEn:       "App Key",
				Type:          engine.FieldText,
				Required:      true,
				Placeholder:   "xxxxxxxxxxxxxxxx",
				Description:   "在有道智云控制台「应用管理」中查看。注意不要与「应用密钥」填反。",
				DescriptionEn: "Find it in Youdao Zhiyun Console → App management. Do not mix it up with the App Secret.",
			},
			{
				Key:           "appSecret",
				Label:         "应用密钥（App Secret）",
				LabelEn:       "App Secret",
				Type:          engine.FieldPassword,
				Secret:        true,
				Required:      true,
				Placeholder:   "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
				Description:   "与应用 ID 成对出现。由系统密钥库加密保存，界面只显示掩码指纹。",
				DescriptionEn: "Pairs with the App Key. Encrypted by the OS keychain; only a masked fingerprint is shown.",
			},
			{
				Key:           "endpoint",
				Label:         "自定义接口地址",
				LabelEn:       "Custom endpoint",
				Type:          engine.FieldText,
				Placeholder:   "留空使用官方端点",
				Description:   "多数情况留空即可。",
				DescriptionEn: "Leave empty in most cases.",
			},
			{
				Key:     "ipWhitelistHint",
				Label:   "IP 白名单提醒",
				LabelEn: "IP allowlist",
				Type:    engine.FieldBoolean,
				Default: false,
				Description: "如果有道应用开启了 IP 访问限制，需把本机出口 IP 加入白名单，否则会报 203。" +
					"此项仅作提醒，不影响请求。",
				DescriptionEn: "If IP restrictions are enabled on your Youdao app, add this machine's egress IP, " +
					"otherwise you will get error 203. Informational only.",
			},
			engine.TimeoutField(engine.DefaultTimeoutSecs),
			engine.ConcurrencyField(1,
				"同时发往有道的最大请求数。请保持 1，否则容易触发 109/411 频率限制。",
				"Maximum concurrent requests. Keep at 1 to avoid 109/411 rate limits."),
			engine.SkipTLSField(),
		},
	}
}
