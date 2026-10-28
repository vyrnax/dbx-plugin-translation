package engine

// 本文件定义「引擎配置表单」的声明式描述。
//
// # 为什么 schema 的单一事实来源必须在侧车
//
// 如果前端硬编码每个引擎的表单字段，那么新增一个引擎就要改前端代码，
// 「新增引擎 = 新增一个包 + 一行 Register」的架构承诺就破产了。
//
// 把 schema 放在侧车后：
//   - 适配器自己声明自己需要哪些字段（字段定义与使用它的代码在同一个包里）
//   - translate/schema 一次性下发全部引擎的表单描述
//   - 前端只有一个【通用渲染器】，永远不需要为新引擎改动
//
// # 字段语义对齐 DBX 的 connection-provider
//
// 字段结构刻意对齐 DBX 官方表单语义（key/label/type/required/default/options/
// description/placeholder/visible_when），这样：
//   - 前端渲染器的实现思路可以直接参考 DBX 宿主的行为
//   - 未来若改用 connection-provider 托管引擎配置，字段定义几乎可以原样迁移
//
// 但【不复用】DBX 的 binding 概念：本项目不用 Secret Store，
// 密钥字段用 Secret=true 标记，由侧车自己的 secret 包加密。

// FieldType 是表单字段的输入类型。
type FieldType string

const (
	FieldText     FieldType = "text"     // 单行文本
	FieldPassword FieldType = "password" // 密码框（前端显示为圆点）
	FieldNumber   FieldType = "number"   // 数字
	FieldBoolean  FieldType = "boolean"  // 开关
	FieldSelect   FieldType = "select"   // 下拉单选
	FieldRadio    FieldType = "radio"    // 单选按钮组
	FieldTextarea FieldType = "textarea" // 多行文本
)

// Option 是 select / radio 的一个选项。
//
// Value 必须是字符串 —— 对齐 DBX 表单语义（"select/radio 必须提供非空 options，
// 每项包含 label 和字符串 value"）。即使语义上是数字或布尔，也用字符串承载，
// 由适配器自己解析。这样前端渲染器不需要处理多种值类型。
type Option struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// VisibleWhen 是条件显隐规则。
//
// 语义对齐 DBX：Field 指定依赖的字段，OneOf 列出命中值（按字符串比较）。
// 引用字段自身处于隐藏状态时该子句不参与判定（级联），未填写或空值永不命中。
//
// 只实现单条子句 + AllOf/AnyOf/Not 组合，不做 DBX 那套 8 层 64 节点的完整求值器 ——
// 本项目的表单最多两层条件（选了引擎类型才显示对应字段），过度设计没有收益。
type VisibleWhen struct {
	Field string        `json:"field,omitempty"`  // 依赖的字段 key
	OneOf []string      `json:"one_of,omitempty"` // 命中值列表（字符串比较）
	AllOf []VisibleWhen `json:"all_of,omitempty"`
	AnyOf []VisibleWhen `json:"any_of,omitempty"`
	Not   *VisibleWhen  `json:"not,omitempty"`
}

// Field 是一个表单字段。
type Field struct {
	Key           string       `json:"key"`               // 字段标识，写入 Config 或 Secrets
	Label         string       `json:"label"`             // 显示名（中文）
	LabelEn       string       `json:"labelEn,omitempty"` // 显示名（英文），前端按 locale 选
	Type          FieldType    `json:"type"`
	Required      bool         `json:"required,omitempty"`
	Secret        bool         `json:"secret,omitempty"` // true 表示这是密钥字段
	Default       any          `json:"default,omitempty"`
	Options       []Option     `json:"options,omitempty"` // select/radio 必填
	Placeholder   string       `json:"placeholder,omitempty"`
	Description   string       `json:"description,omitempty"`
	DescriptionEn string       `json:"descriptionEn,omitempty"`
	VisibleWhen   *VisibleWhen `json:"visible_when,omitempty"`
	Min           *float64     `json:"min,omitempty"`       // number 的下限
	Max           *float64     `json:"max,omitempty"`       // number 的上限
	Multiline     int          `json:"multiline,omitempty"` // textarea 的行数
}

// Schema 是一个引擎类型的完整表单描述。
type Schema struct {
	Kind  string `json:"kind"`  // 引擎类型标识（Register 时自动填入）
	Label string `json:"label"` // 显示名（中文）
	// Icon 是该引擎图标的文件名（相对 UI 的 icons/ 目录，例如 "deepl.svg"）。
	//
	// 为什么由引擎声明、而不是前端按 kind 拼路径硬编码：
	//   前端的承诺是「新增引擎零改动」。若前端维护一张 icons/<kind>.svg 映射表，
	//   就等于要求文件名永远等于 kind 名 —— 而本项目实际不是：阿里云的图标文件
	//   叫 ali.svg、本地 CLI 的叫 cli.svg。映射权交给引擎自己，前端只做通用渲染。
	//
	// 为什么只存文件名而不是完整路径：
	//   宿主的 ui.readAsset 只能读 UI root 内的文件（其实现里明确抛
	//   "Asset is outside UI root"），所以图标必须放在 ui/icons/ 下。
	//   这个目录位置属于前端实现细节，不该泄漏进后端契约，由前端自己加前缀。
	//
	// 留空时前端退化为「首字母色块」，不报错、布局不抖。
	Icon    string `json:"icon,omitempty"`
	LabelEn string `json:"labelEn,omitempty"` // 显示名（英文）
	// DefaultName / DefaultNameEn 是新建该类型引擎时【自动填入】的名称。
	//
	// 为什么放在 schema 里而不是前端硬编码：
	//   同一类型可以添加多个实例（例如两个不同的 OpenAI 兼容端点），
	//   用户必须靠名称区分它们。默认名要能一眼看出是哪家服务
	//   （「阿里云翻译」而不是「aliyun」），这属于引擎自身的知识，
	//   应该和字段定义放在一起。前端硬编码会破坏「新增引擎前端零改动」。
	//
	// 前端行为：选了类型就填入默认名；用户改过名称后【不再覆盖】（脏值跟踪）；
	//   切换类型时，若名称仍等于上一个类型的默认名则跟着换，否则保留用户输入。
	DefaultName   string  `json:"defaultName,omitempty"`
	DefaultNameEn string  `json:"defaultNameEn,omitempty"`
	Description   string  `json:"description,omitempty"`
	DescriptionEn string  `json:"descriptionEn,omitempty"`
	Streaming     bool    `json:"streaming"` // 是否支持流式输出
	MaxChars      int     `json:"maxChars"`  // 单次请求字符上限
	Fields        []Field `json:"fields"`
	// NeedsSecret 表示该引擎必须配置密钥。
	// localcli 是 false（CLI 自己持有凭据），其余都是 true。
	NeedsSecret bool `json:"needsSecret"`
	// SecretLabel 是密钥字段的显示名（各引擎叫法不同：API Key / Secret Key / App Secret）
	SecretLabel string `json:"secretLabel,omitempty"`
}

// 以下字段构造器一律【导出】：各引擎子包（engine/openai、engine/deepl …）
// 都要复用同一套字段定义。如果留在 engine 包内部（小写），子包就看不见，
// 只能各自复制一份 —— 那正是「六份定义逐渐漂移」的开端。
//
// 复用的价值不只是少写代码：超时上限为什么是 600 秒、并发为什么上限 4、
// 跳过 TLS 校验为什么必须警示，这些理由只在构造器的注释里写一次，
// 六个引擎共享同一份解释，改一处就全改到了。

// NumPtr 是构造 Min/Max 指针的辅助函数。
//
// 用指针而非值：JSON 里 omitempty 对 0 值会省略字段，
// 而「下限恰好是 0」是合法配置（例如 chunkDelayMs 可以为 0），
// 用指针才能区分「未设置」与「设置为 0」。
func NumPtr(v float64) *float64 { return &v }

// ---------------------------------------------------------------------------
// 通用字段构造器
// ---------------------------------------------------------------------------

// EndpointField 构造「接口地址」字段。
//
// 所有 HTTP 类引擎都需要它，且校验规则一致（必须 http/https 开头），
// 抽出来避免六份重复定义漂移。
func EndpointField(label, labelEn, placeholder, desc, descEn string) Field {
	return Field{
		Key:           "endpoint",
		Label:         label,
		LabelEn:       labelEn,
		Type:          FieldText,
		Required:      true,
		Placeholder:   placeholder,
		Description:   desc,
		DescriptionEn: descEn,
	}
}

// TimeoutField 构造「超时秒数」字段。
//
// 上限 600 秒：再长的超时没有实际意义，而且会让「卡住」和「慢」无法区分。
// 下限 5 秒：低于这个值连正常的 TLS 握手都可能完不成。
func TimeoutField(def int) Field {
	return Field{
		Key:           "timeoutSecs",
		Label:         "超时（秒）",
		LabelEn:       "Timeout (seconds)",
		Type:          FieldNumber,
		Default:       def,
		Min:           NumPtr(5),
		Max:           NumPtr(600),
		Description:   "单次请求的最长等待时间。流式输出指「首个字节」的等待上限。",
		DescriptionEn: "Maximum wait time per request. For streaming, this is the time-to-first-byte limit.",
	}
}

// SkipTLSField 构造「跳过证书校验」字段。
//
// 描述里必须写明风险 —— 这个选项会让连接可被中间人攻击，
// 用户只有在自签名证书的内网环境下才应该开启。
func SkipTLSField() Field {
	return Field{
		Key:           "skipTlsVerify",
		Label:         "跳过 TLS 证书校验",
		LabelEn:       "Skip TLS certificate verification",
		Type:          FieldBoolean,
		Default:       false,
		Description:   "危险选项：仅用于自签名证书的内网端点。开启后连接可被中间人攻击。",
		DescriptionEn: "Dangerous: only for internal endpoints with self-signed certificates. Enables MITM attacks.",
	}
}

// ConcurrencyField 构造「并发数」字段。
//
// 默认 1：百度/有道免费档是 1 QPS，并发高了直接触发限流。
// 上限 4：再高对翻译场景没有收益（用户不会同时发 10 个翻译请求），
// 却会显著增加触发限流的概率。
func ConcurrencyField(def int, desc, descEn string) Field {
	return Field{
		Key:           "concurrency",
		Label:         "并发请求数",
		LabelEn:       "Concurrent requests",
		Type:          FieldNumber,
		Default:       def,
		Min:           NumPtr(1),
		Max:           NumPtr(4),
		Description:   desc,
		DescriptionEn: descEn,
	}
}

// PromptField 构造「自定义 Prompt」字段（仅 LLM 类引擎）。
//
// 用 textarea 且非必填：留空时用内置模板。
// 内置模板已经过调优（强调「只输出译文」「保留格式」），
// 大多数用户不需要改；但专业场景（例如要求特定术语表）需要覆盖能力。
func PromptField() Field {
	return Field{
		Key:           "promptTemplate",
		Label:         "自定义 Prompt 模板",
		LabelEn:       "Custom prompt template",
		Type:          FieldTextarea,
		Multiline:     5,
		Placeholder:   "留空使用内置模板。可用占位符：{targetLang} {sourceLang} {text}",
		Description:   "留空时使用内置翻译模板。支持占位符：{targetLang} 目标语言、{sourceLang} 源语言、{text} 待译文本。",
		DescriptionEn: "Leave empty to use the built-in template. Placeholders: {targetLang}, {sourceLang}, {text}.",
	}
}

// MaxCharsField 构造「单次请求字符上限」字段（仅 LLM 类引擎，可覆盖默认值）。
func MaxCharsField(def int) Field {
	return Field{
		Key:           "maxChars",
		Label:         "单次请求字符上限",
		LabelEn:       "Max characters per request",
		Type:          FieldNumber,
		Default:       def,
		Min:           NumPtr(500),
		Max:           NumPtr(100000),
		Description:   "超过此长度的文本会被自动分段翻译。上下文窗口小的模型应调低此值。",
		DescriptionEn: "Longer text is automatically split. Lower this for models with small context windows.",
	}
}
