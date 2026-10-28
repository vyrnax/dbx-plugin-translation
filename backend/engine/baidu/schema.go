package baidu

import "github.com/vyrnax/translation/engine"

// schema 返回百度翻译的配置表单描述。
//
// 注意 baidu 与其它引擎的差异：它有 AppID 与「密钥」两个凭据，
// 且 AppID 不是敏感信息（明文 config 即可），密钥才是 secret。
func schema() engine.Schema {
	return engine.Schema{

		Kind:          Kind,
		Icon:          "baidu.svg",
		Label:         "百度翻译",
		LabelEn:       "Baidu Translate",
		DefaultName:   "百度翻译",
		DefaultNameEn: "Baidu Translate",
		Description: "百度翻译开放平台「通用文本翻译」。标准版免费额度为 1 QPS、5 万字符/月；" +
			"高级版按量计费。支持 200+ 语种，中文与亚洲语言互译质量较好。不支持流式输出。",
		DescriptionEn: "Baidu Translate Open Platform (general text translation). Free tier: 1 QPS, " +
			"50K chars/month. Strong for Chinese and Asian languages. No streaming.",
		Streaming:   false,
		MaxChars:    maxCharsPerRequest,
		NeedsSecret: true,
		SecretLabel: "密钥（Secret Key）",
		Fields: []engine.Field{
			{
				Key:           "appId",
				Label:         "AppID",
				LabelEn:       "AppID",
				Type:          engine.FieldText,
				Required:      true,
				Placeholder:   "20200101000000001",
				Description:   "在百度翻译开放平台「管理控制台 → 开发者信息」中查看。注意不要与「密钥」填反。",
				DescriptionEn: "Find it in Baidu Translate Open Platform → Developer info. Do not mix it up with the secret key.",
			},
			{
				Key:           "secretKey",
				Label:         "密钥（Secret Key）",
				LabelEn:       "Secret Key",
				Type:          engine.FieldPassword,
				Secret:        true,
				Required:      true,
				Placeholder:   "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
				Description:   "与 AppID 成对出现。由系统密钥库加密保存，界面只显示掩码指纹。",
				DescriptionEn: "Pairs with the AppID. Encrypted by the OS keychain; only a masked fingerprint is shown.",
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
				Description: "如果你的百度账号开启了 IP 访问限制，需要把本机出口 IP 加入白名单，否则会报 58000。" +
					"此项仅作提醒，不影响请求。",
				DescriptionEn: "If your Baidu account has IP restrictions enabled, add this machine's egress IP to the allowlist, " +
					"otherwise you will get error 58000. This toggle is informational only.",
			},
			engine.TimeoutField(engine.DefaultTimeoutSecs),
			engine.ConcurrencyField(1,
				"同时发往百度的最大请求数。标准版免费额度为 1 QPS，请务必保持 1，否则会报 54003 频率受限。",
				"Maximum concurrent requests. The free tier is 1 QPS; keep at 1 to avoid error 54003."),
			engine.SkipTLSField(),
		},
	}
}
