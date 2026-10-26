package aliyun

import "github.com/vyrnax/translation/engine"

// schema 返回阿里云翻译的配置表单描述。
func schema() engine.Schema {
	return engine.Schema{
		Kind:          Kind,
		Label:         "阿里云翻译",
		LabelEn:       "Alibaba Cloud Translate",
		DefaultName:   "阿里云翻译",
		DefaultNameEn: "Alibaba Cloud Translate",
		Description: "阿里云机器翻译（TranslateGeneral，2018-10-12）。需在阿里云 RAM 创建 AccessKey " +
			"并授予机器翻译权限。支持 200+ 语种与多个行业场景（通用/标题/描述/口语/医疗/社交）。不支持流式输出。",
		DescriptionEn: "Alibaba Cloud Machine Translation (TranslateGeneral, 2018-10-12). Create an AccessKey " +
			"in RAM with MT permissions. Supports 200+ languages and multiple scenes. No streaming.",
		Streaming:   false,
		MaxChars:    maxCharsPerRequest,
		NeedsSecret: true,
		SecretLabel: "AccessKey Secret",
		Fields: []engine.Field{
			{
				Key:           "accessKeyId",
				Label:         "AccessKey ID",
				LabelEn:       "AccessKey ID",
				Type:          engine.FieldText,
				Required:      true,
				Placeholder:   "LTAI5t...",
				Description:   "在阿里云 RAM 控制台「用户 → 创建 AccessKey」获得。建议为该用户单独授予机器翻译权限。",
				DescriptionEn: "Create in Alibaba Cloud RAM Console → Users → Create AccessKey. Grant only MT permissions.",
			},
			{
				Key:           "accessKeySecret",
				Label:         "AccessKey Secret",
				LabelEn:       "AccessKey Secret",
				Type:          engine.FieldPassword,
				Secret:        true,
				Required:      true,
				Placeholder:   "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
				Description:   "与 AccessKey ID 成对出现，只在创建时显示一次。由系统密钥库加密保存。",
				DescriptionEn: "Pairs with the AccessKey ID and is shown only once at creation. Encrypted by the OS keychain.",
			},
			{
				Key:     "region",
				Label:   "地域",
				LabelEn: "Region",
				Type:    engine.FieldSelect,
				Default: "cn-hangzhou",
				Options: []engine.Option{
					{Label: "华东 1（杭州）", Value: "cn-hangzhou"},
					{Label: "华东 2（上海）", Value: "cn-shanghai"},
					{Label: "华北 2（北京）", Value: "cn-beijing"},
					{Label: "华南 1（深圳）", Value: "cn-shenzhen"},
					{Label: "新加坡", Value: "ap-southeast-1"},
				},
				Description:   "选择离你较近的地域以降低延迟。填了自定义接口地址时此项被忽略。",
				DescriptionEn: "Pick the nearest region for lower latency. Ignored when a custom endpoint is set.",
			},
			{
				Key:     "scene",
				Label:   "翻译场景",
				LabelEn: "Translation scene",
				Type:    engine.FieldSelect,
				Default: "general",
				Options: []engine.Option{
					{Label: "通用", Value: "general"},
					{Label: "标题", Value: "title"},
					{Label: "产品描述", Value: "description"},
					{Label: "口语 / 对话", Value: "communication"},
					{Label: "医疗", Value: "medical"},
					{Label: "社交", Value: "social"},
				},
				Description:   "场景影响译文的用词风格。翻译数据库注释建议用「通用」，翻译界面文案可用「社交」。",
				DescriptionEn: "The scene affects wording style. Use general for DB comments, social for UI copy.",
			},
			{
				Key:           "endpoint",
				Label:         "自定义接口地址",
				LabelEn:       "Custom endpoint",
				Type:          engine.FieldText,
				Placeholder:   "留空按地域自动生成",
				Description:   "多数情况留空即可，会自动使用 https://mt.<地域>.aliyuncs.com。",
				DescriptionEn: "Leave empty in most cases; https://mt.<region>.aliyuncs.com is used automatically.",
			},
			engine.TimeoutField(engine.DefaultTimeoutSecs),
			engine.ConcurrencyField(1,
				"同时发往阿里云的最大请求数。请保持 1，避免触发限流。",
				"Maximum concurrent requests. Keep at 1 to avoid throttling."),
			engine.SkipTLSField(),
		},
	}
}
