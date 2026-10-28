package tencent

import "github.com/vyrnax/translation/engine"

// schema 返回腾讯云翻译的配置表单描述。
func schema() engine.Schema {
	return engine.Schema{

		Kind:          Kind,
		Icon:          "tencent.svg",
		Label:         "腾讯云翻译",
		LabelEn:       "Tencent Cloud TMT",
		DefaultName:   "腾讯云翻译",
		DefaultNameEn: "Tencent Cloud Translate",
		Description: "腾讯云机器翻译（TMT TextTranslate，2018-03-21）。需在腾讯云 CAM 创建 SecretId/SecretKey " +
			"并开通机器翻译服务。每月 500 万字符免费额度。不支持流式输出。",
		DescriptionEn: "Tencent Cloud Machine Translation (TMT TextTranslate, 2018-03-21). Create SecretId/SecretKey " +
			"in CAM and enable TMT. 5M chars/month free. No streaming.",
		Streaming:   false,
		MaxChars:    maxCharsPerRequest,
		NeedsSecret: true,
		SecretLabel: "SecretKey",
		Fields: []engine.Field{
			{
				Key:           "secretId",
				Label:         "SecretId",
				LabelEn:       "SecretId",
				Type:          engine.FieldText,
				Required:      true,
				Placeholder:   "AKIDxxxxxxxxxxxxxxxxxxxxxxxx",
				Description:   "在腾讯云控制台「访问管理 → API 密钥管理」创建。注意不要与 SecretKey 填反。",
				DescriptionEn: "Create in Tencent Cloud Console → CAM → API key management. Do not mix it up with SecretKey.",
			},
			{
				Key:           "secretKey",
				Label:         "SecretKey",
				LabelEn:       "SecretKey",
				Type:          engine.FieldPassword,
				Secret:        true,
				Required:      true,
				Placeholder:   "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
				Description:   "与 SecretId 成对出现，只在创建时显示一次。由系统密钥库加密保存。",
				DescriptionEn: "Pairs with the SecretId and is shown only once. Encrypted by the OS keychain.",
			},
			{
				Key:     "region",
				Label:   "地域",
				LabelEn: "Region",
				Type:    engine.FieldSelect,
				Default: "ap-guangzhou",
				Options: []engine.Option{
					{Label: "广州", Value: "ap-guangzhou"},
					{Label: "上海", Value: "ap-shanghai"},
					{Label: "北京", Value: "ap-beijing"},
					{Label: "成都", Value: "ap-chengdu"},
					{Label: "中国香港", Value: "ap-hongkong"},
				},
				Description:   "选择离你较近的地域以降低延迟。",
				DescriptionEn: "Pick the nearest region for lower latency.",
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
			engine.TimeoutField(engine.DefaultTimeoutSecs),
			engine.ConcurrencyField(1,
				"同时发往腾讯云的最大请求数。请保持 1，避免触发频率限制。",
				"Maximum concurrent requests. Keep at 1 to avoid rate limits."),
			engine.SkipTLSField(),
		},
	}
}
