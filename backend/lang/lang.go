// Package lang 管理「规范语言码」与各翻译引擎私有语言码之间的映射。
//
// 为什么需要这一层：
//
//	同一个语言在不同引擎里的写法完全不同 —— 简体中文在 DeepL 是 `ZH`、
//	在百度是 `zh`、在有道是 `zh-CHS`、在阿里是 `zh`；英语在 DeepL 是 `EN-US`、
//	在百度是 `en`、在有道是 `en`。如果让每个适配器自己处理，映射逻辑会散落到
//	六个包里，改一处要改六处，而且极易出现「某引擎悄悄不支持某语言」的漏洞。
//
//	集中到本包后：
//	  - 前端只需认识规范码，下拉框选项由 translate/schema 统一下发；
//	  - 某引擎不支持某语言时 Map 返回 ok=false，UI 据此【禁用】该选项，
//	    而不是等用户选了之后翻译失败；
//	  - 新增引擎只需在本包加一张映射表。
//
// `auto`（自动检测源语言）是特殊值：它不是真实语言，只有【源语言】位置可用，
// 且并非所有引擎都支持（DeepL 支持，百度/有道/阿里的部分接口不支持）。
package lang

// Auto 是「自动检测源语言」的规范码。
const Auto = "auto"

// Language 描述一种规范语言。
type Language struct {
	Code string `json:"code"` // 规范码，如 "zh"
	Zh   string `json:"zh"`   // 中文名，用于 zh-CN 界面
	En   string `json:"en"`   // 英文名，用于 en 界面
	LLM  string `json:"llm"`  // 喂给大模型的自然语言名（Prompt 里用这个，模型理解得最准）
}

// canonical 是全部规范语言，顺序即 UI 下拉框的展示顺序。
//
// 排序原则：auto 置顶，然后中文（简/繁）、英语、日语、韩语等东亚语言优先，
// 其余按使用广度排列。这个顺序对中文用户最友好。
var canonical = []Language{
	{Auto, "自动检测", "Auto detect", "the source language (auto-detect)"},

	{"zh", "简体中文", "Chinese (Simplified)", "Simplified Chinese"},
	{"zh-TW", "繁体中文", "Chinese (Traditional)", "Traditional Chinese"},
	{"en", "英语", "English", "English"},
	{"ja", "日语", "Japanese", "Japanese"},
	{"ko", "韩语", "Korean", "Korean"},

	{"fr", "法语", "French", "French"},
	{"de", "德语", "German", "German"},
	{"es", "西班牙语", "Spanish", "Spanish"},
	{"ru", "俄语", "Russian", "Russian"},
	{"pt", "葡萄牙语", "Portuguese", "Portuguese"},
	{"it", "意大利语", "Italian", "Italian"},

	{"ar", "阿拉伯语", "Arabic", "Arabic"},
	{"th", "泰语", "Thai", "Thai"},
	{"vi", "越南语", "Vietnamese", "Vietnamese"},
	{"id", "印尼语", "Indonesian", "Indonesian"},
	{"ms", "马来语", "Malay", "Malay"},
	{"tr", "土耳其语", "Turkish", "Turkish"},
	{"hi", "印地语", "Hindi", "Hindi"},

	{"pl", "波兰语", "Polish", "Polish"},
	{"nl", "荷兰语", "Dutch", "Dutch"},
	{"sv", "瑞典语", "Swedish", "Swedish"},
	{"da", "丹麦语", "Danish", "Danish"},
	{"fi", "芬兰语", "Finnish", "Finnish"},
	{"no", "挪威语", "Norwegian", "Norwegian"},
	{"cs", "捷克语", "Czech", "Czech"},
	{"el", "希腊语", "Greek", "Greek"},
	{"he", "希伯来语", "Hebrew", "Hebrew"},
	{"uk", "乌克兰语", "Ukrainian", "Ukrainian"},
	{"ro", "罗马尼亚语", "Romanian", "Romanian"},
	{"hu", "匈牙利语", "Hungarian", "Hungarian"},
}

// All 返回全部规范语言（含 auto），供 translate/schema 下发给前端。
//
// 返回副本而非内部切片，避免调用方意外修改包内状态。
func All() []Language {
	out := make([]Language, len(canonical))
	copy(out, canonical)
	return out
}

// Codes 返回全部规范语言码（含 auto）。
func Codes() []string {
	out := make([]string, 0, len(canonical))
	for _, l := range canonical {
		out = append(out, l.Code)
	}
	return out
}

// IsValid 判断是否为合法规范码（含 auto）。
func IsValid(code string) bool {
	for _, l := range canonical {
		if l.Code == code {
			return true
		}
	}
	return false
}

// Lookup 按规范码查语言定义。找不到时 ok=false。
func Lookup(code string) (Language, bool) {
	for _, l := range canonical {
		if l.Code == code {
			return l, true
		}
	}
	return Language{}, false
}

// LLMName 返回喂给大模型的自然语言名。
//
// 未知码时回退为码本身 —— 大模型对 "zh-TW" 这类标准 BCP-47 标签通常也能理解，
// 比直接报错更实用。
func LLMName(code string) string {
	if l, ok := Lookup(code); ok {
		return l.LLM
	}
	return code
}

// ZhName 返回中文名，未知码回退为码本身。
func ZhName(code string) string {
	if l, ok := Lookup(code); ok {
		return l.Zh
	}
	return code
}

// EnName 返回英文名，未知码回退为码本身。
func EnName(code string) string {
	if l, ok := Lookup(code); ok {
		return l.En
	}
	return code
}

// ---------------------------------------------------------------------------
// 各引擎的私有语言码映射表
// ---------------------------------------------------------------------------
//
// 约定：
//   - 表里【没有】的规范码 = 该引擎不支持，Map 返回 ok=false。
//   - 源语言位置额外支持 Auto 时，必须在表里显式写出 Auto 的映射，
//     否则「自动检测」会被判为不支持。
//   - 目标语言位置永远不该出现 Auto（翻译成"自动检测"没有意义），
//     由 MapTarget 单独拦截。

// openaiMap 是 OpenAI 兼容大模型的映射。
//
// 大模型不需要私有语言码 —— Prompt 里直接用自然语言名（LLM 字段）表达最准确，
// 模型对 "Simplified Chinese" 的理解远好于对 "zh-CN" 这类标签的理解。
// 因此这里把每个规范码映射到它自己的 LLM 名，且【全部语言都支持】：
// 大模型的能力边界由模型本身决定，不由我们预先限制。
var openaiMap = buildLLMMap()

// localcliMap 与 openai 同理：本地 CLI 背后也是大模型。
var localcliMap = buildLLMMap()

// freeMap 同理：内置免费引擎背后也是大模型（B站 Index-Translate），
// 语言靠 Prompt 里的自然语言名表达，不需要私有码表。
var freeMap = buildLLMMap()

// deeplMap 是 DeepL 的映射。DeepL 用大写码，且英语/葡萄牙语必须带地区后缀。
//
// 注意 DeepL 的 auto 只在【源语言】位置有效（target_lang 不接受 auto），
// 这里仍然登记它，由 MapSource/MapTarget 分别处理。
var deeplMap = map[string]string{
	Auto: "", // DeepL 源语言留空即表示自动检测
	"zh": "ZH",
	"en": "EN-US", // EN-US 比 EN-GB 更通用；用户如需英式可自行改配置
	"ja": "JA",
	"ko": "KO",
	"fr": "FR",
	"de": "DE",
	"es": "ES",
	"ru": "RU",
	"pt": "PT-PT",
	"it": "IT",
	"ar": "AR",
	"th": "TH",
	"vi": "VI",
	"id": "ID",
	"tr": "TR",
	"hi": "HI",
	"pl": "PL",
	"nl": "NL",
	"sv": "SV",
	"da": "DA",
	"fi": "FI",
	"no": "NB", // DeepL 用 NB（书面挪威语）表示挪威语
	"cs": "CS",
	"el": "EL",
	"he": "HE",
	"uk": "UK",
	"ro": "RO",
	"hu": "HU",
	// DeepL 不支持：zh-TW（只有 ZH）、ms
}

// baiduMap 是百度翻译开放平台的映射（通用文本翻译接口）。
//
// 百度的码系自成一套：日语是 jp（不是 ja）、韩语是 kor（不是 ko）、
// 繁体是 cht、文言文是 wyw、粤语是 yue。
// 百度【不支持】源语言自动检测以外的 auto 语义，但支持 src=auto。
var baiduMap = map[string]string{
	Auto:    "auto",
	"zh":    "zh",
	"zh-TW": "cht",
	"en":    "en",
	"ja":    "jp",
	"ko":    "kor",
	"fr":    "fra",
	"de":    "de",
	"es":    "spa",
	"ru":    "ru",
	"pt":    "pt",
	"it":    "it",
	"ar":    "ara",
	"th":    "th",
	"vi":    "vie",
	"id":    "id",
	"ms":    "may",
	"tr":    "tr",
	"hi":    "hi",
	"pl":    "pl",
	"nl":    "nl",
	"sv":    "swe",
	"da":    "dan",
	"fi":    "fin",
	"no":    "nor",
	"cs":    "cs",
	"el":    "el",
	"he":    "heb",
	"uk":    "ukr",
	"ro":    "rom",
	"hu":    "hu",
}

// youdaoMap 是有道智云的映射。
//
// 有道的简体中文是 zh-CHS、繁体是 zh-CHT（不是 zh / zh-TW）。
// 有道【不支持】 auto，源语言必须显式指定 —— 这是它与其他引擎的重要差异，
// 因此表里没有 Auto 键，MapSource 会返回 ok=false，UI 据此禁用「自动检测」。
var youdaoMap = map[string]string{
	"zh":    "zh-CHS",
	"zh-TW": "zh-CHT",
	"en":    "en",
	"ja":    "ja",
	"ko":    "ko",
	"fr":    "fr",
	"de":    "de",
	"es":    "es",
	"ru":    "ru",
	"pt":    "pt",
	"it":    "it",
	"ar":    "ar",
	"th":    "th",
	"vi":    "vi",
	"id":    "id",
	"ms":    "ms",
	"tr":    "tr",
	"hi":    "hi",
	"pl":    "pl",
	"nl":    "nl",
	"sv":    "sv",
	"da":    "da",
	"fi":    "fi",
	"no":    "no",
	"cs":    "cs",
	"el":    "el",
	"he":    "he",
	"uk":    "uk",
	"ro":    "ro",
	"hu":    "hu",
}

// aliyunMap 是阿里云机器翻译（TranslateGeneral）的映射。
//
// 阿里用标准 ISO 639-1 风格小写码，简体中文是 zh、繁体是 zh-tw。
// 阿里支持 sourceLanguage=auto。
var aliyunMap = map[string]string{
	Auto:    "auto",
	"zh":    "zh",
	"zh-TW": "zh-tw",
	"en":    "en",
	"ja":    "ja",
	"ko":    "ko",
	"fr":    "fr",
	"de":    "de",
	"es":    "es",
	"ru":    "ru",
	"pt":    "pt",
	"it":    "it",
	"ar":    "ar",
	"th":    "th",
	"vi":    "vi",
	"id":    "id",
	"ms":    "ms",
	"tr":    "tr",
	"hi":    "hi",
	"pl":    "pl",
	"nl":    "nl",
	"sv":    "sv",
	"da":    "da",
	"fi":    "fi",
	"no":    "no",
	"cs":    "cs",
	"el":    "el",
	"he":    "he",
	"uk":    "uk",
	"ro":    "ro",
	"hu":    "hu",
}

// googleMap 是 Google Cloud Translation API (v2) 的映射。
//
// Google 用标准 BCP-47 标签，但有几个特例：
//   - 简体中文是 zh-CN、繁体是 zh-TW（不是 zh / zh-Hant）
//   - 希伯来语官方用 he（旧代码 iw 已废弃但仍被接受）
//   - 挪威语用 no（Google 不接受 nb）
//
// 支持 sourceLanguage=（留空）表示自动检测。
var googleMap = map[string]string{
	Auto:    "", // Google 用「省略 sourceLanguage」表示自动检测
	"zh":    "zh-CN",
	"zh-TW": "zh-TW",
	"en":    "en",
	"ja":    "ja",
	"ko":    "ko",
	"fr":    "fr",
	"de":    "de",
	"es":    "es",
	"ru":    "ru",
	"pt":    "pt",
	"it":    "it",
	"ar":    "ar",
	"th":    "th",
	"vi":    "vi",
	"id":    "id",
	"ms":    "ms",
	"tr":    "tr",
	"hi":    "hi",
	"pl":    "pl",
	"nl":    "nl",
	"sv":    "sv",
	"da":    "da",
	"fi":    "fi",
	"no":    "no",
	"cs":    "cs",
	"el":    "el",
	"he":    "he",
	"uk":    "uk",
	"ro":    "ro",
	"hu":    "hu",
}

// tencentMap 是腾讯云机器翻译（TMT）的映射。
//
// 腾讯用 ISO 639-1 风格小写码，但有几个自有写法：
//   - 简体中文 zh、繁体 zh-TW（与阿里一致）
//   - 源语言支持 auto
//
// 腾讯的语种覆盖比百度/有道窄一些（约 18 种），未列出的语言
// 会由 Map 返回 ok=false，UI 据此禁用下拉项而不是等翻译失败。
var tencentMap = map[string]string{
	Auto:    "auto",
	"zh":    "zh",
	"zh-TW": "zh-TW",
	"en":    "en",
	"ja":    "ja",
	"ko":    "ko",
	"fr":    "fr",
	"de":    "de",
	"es":    "es",
	"ru":    "ru",
	"pt":    "pt",
	"it":    "it",
	"ar":    "ar",
	"th":    "th",
	"vi":    "vi",
	"id":    "id",
	"ms":    "ms",
	"tr":    "tr",
	"hi":    "hi",
}

// sourceOverrides 是【源语言位置】专用的映射覆盖表。
//
// 为什么需要它：某些引擎的 source_lang 与 target_lang 接受的取值集合【不同】。
//
// DeepL 是最典型的例子：
//
//	target_lang 接受 EN-US / EN-GB / PT-PT / PT-BR（必须带地区后缀，
//	          因为要区分美式英式、欧葡巴葡）
//	source_lang 只接受 EN / PT（【不带】地区后缀，因为源语言只需识别到语种）
//
// 共用一张表的后果是：把英语作为【源语言】时会发出 source_lang=EN-US，
// DeepL 直接返回 400 "Value for 'source_lang' not supported"。
// 这个 bug 的隐蔽之处在于 —— 英译中会失败，但中译英正常
// （因为中文 ZH 在两个位置都合法），看起来像"偶发"故障。
//
// 本项目实测踩过：用户填了正确的 DeepL 免费 Key，点「测试连接」报 400，
// 而用官方 curl 示例（不带 source_lang）却能成功。
var sourceOverrides = map[string]map[string]string{
	"deepl": {
		"en": "EN", // 不是 EN-US
		"pt": "PT", // 不是 PT-PT
	},
}

// maps 是引擎 kind → 映射表的总索引。新增引擎时在这里加一行即可。
var maps = map[string]map[string]string{
	"openai":   openaiMap,
	"localcli": localcliMap,
	"free":     freeMap,
	"deepl":    deeplMap,
	"google":   googleMap,
	"baidu":    baiduMap,
	"youdao":   youdaoMap,
	"aliyun":   aliyunMap,
	"tencent":  tencentMap,
}

// buildLLMMap 为大模型类引擎构造「全语言支持」的映射表。
//
// 值直接用 LLM 自然语言名，因为大模型对自然语言名的理解优于对标签码的理解。
// auto 映射为固定提示语，供 Prompt 拼接。
func buildLLMMap() map[string]string {
	m := make(map[string]string, len(canonical))
	for _, l := range canonical {
		if l.Code == Auto {
			m[Auto] = "the source language (auto-detect)"
			continue
		}
		m[l.Code] = l.LLM
	}
	return m
}

// Map 返回规范码在指定引擎下的私有码。
//
// ok=false 的三种情况：
//  1. kind 未注册（引擎不存在）
//  2. code 不是合法规范码
//  3. 该引擎不支持这个语言
//
// 调用方必须检查 ok，不能假设一定成功。
func Map(kind, code string) (string, bool) {
	m, exists := maps[kind]
	if !exists {
		return "", false
	}
	if !IsValid(code) {
		return "", false
	}
	v, ok := m[code]
	if !ok {
		return "", false
	}
	return v, true
}

// MapSource 映射【源语言】。允许 auto（若该引擎支持）。
func MapSource(kind, code string) (string, bool) {
	// 先查源语言专用覆盖表（例如 DeepL 的 en → EN 而非 EN-US）
	if ov, ok := sourceOverrides[kind]; ok {
		if v, hit := ov[code]; hit {
			return v, true
		}
	}
	return Map(kind, code)
}

// MapTarget 映射【目标语言】。
//
// 显式拒绝 auto —— 把文本翻译成"自动检测"没有意义。
// 这个检查放在这里而不是各适配器里，避免六个引擎各写一遍、漏写一个。
func MapTarget(kind, code string) (string, bool) {
	if code == Auto {
		return "", false
	}
	return Map(kind, code)
}

// Supports 判断引擎是否支持某规范码（不区分源/目标）。
func Supports(kind, code string) bool {
	_, ok := Map(kind, code)
	return ok
}

// SupportsAutoSource 判断引擎是否支持源语言自动检测。
//
// 有道不支持（其 API 要求显式 from 语言），其余五家支持。
func SupportsAutoSource(kind string) bool {
	_, ok := Map(kind, Auto)
	return ok
}

// SupportedLangs 返回引擎支持的规范码列表（含 auto，若支持）。
//
// 顺序与 canonical 一致，保证 UI 下拉框顺序稳定。
// 引擎未注册时返回空切片（不是 nil），便于前端直接遍历。
func SupportedLangs(kind string) []string {
	out := make([]string, 0, len(canonical))
	for _, l := range canonical {
		if Supports(kind, l.Code) {
			out = append(out, l.Code)
		}
	}
	return out
}

// SupportedTargetLangs 返回引擎支持的【目标语言】规范码列表（不含 auto）。
func SupportedTargetLangs(kind string) []string {
	out := make([]string, 0, len(canonical))
	for _, l := range canonical {
		if l.Code == Auto {
			continue
		}
		if Supports(kind, l.Code) {
			out = append(out, l.Code)
		}
	}
	return out
}

// KnownKinds 返回已注册映射表的引擎 kind 列表（顺序稳定，便于测试断言）。
func KnownKinds() []string {
	// 顺序即 UI 里引擎类型下拉框的展示顺序：
	// 大模型类在前（覆盖面最广、能力最强），传统机翻按国内使用广度排列。
	order := []string{"openai", "localcli", "free", "deepl", "google", "baidu", "youdao", "aliyun", "tencent"}
	out := make([]string, 0, len(order))
	for _, k := range order {
		if _, ok := maps[k]; ok {
			out = append(out, k)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 文字体系检测与「方向自适应」
// ---------------------------------------------------------------------------

// 文字体系分组。
//
// 为什么按「文字体系」而不是按「语言」判：方向自适应要解决的是
// 「目标是中文、用户却贴进来一段中文」—— 这时引擎（尤其传统 MT）会把原文原样吐回。
// 而 en/de/fr/es 同属拉丁体系，把 Bonjour 译成英文是正常需求，不该被当成冲突。
// 用体系分组判冲突，误伤面比 IDEA 插件那条
// 「含非拉丁字符就一律译成英文」更小（后者对目标已是 en 的拉丁文本反而不生效）。
const (
	ScriptLatin      = "latin"
	ScriptHan        = "han" // 汉字（中日共用，故日语靠假名单独判）
	ScriptKana       = "kana"
	ScriptHangul     = "hangul"
	ScriptCyrillic   = "cyrillic"
	ScriptArabic     = "arabic"
	ScriptHebrew     = "hebrew"
	ScriptThai       = "thai"
	ScriptDevanagari = "devanagari"
	ScriptGreek      = "greek"
)

// scripts 是规范语言码 → 文字体系；未列出的按拉丁处理
// （fr/de/es/pt/it/vi/id/ms/tr/pl/nl/sv/da/fi/no/cs/ro/hu 都用拉丁字母）。
var scripts = map[string]string{
	"zh":    ScriptHan,
	"zh-TW": ScriptHan,
	"ja":    ScriptKana,
	"ko":    ScriptHangul,
	"ru":    ScriptCyrillic,
	"uk":    ScriptCyrillic,
	"ar":    ScriptArabic,
	"he":    ScriptHebrew,
	"th":    ScriptThai,
	"hi":    ScriptDevanagari,
	"el":    ScriptGreek,
	"auto":  "",
}

// Script 返回规范语言码的文字体系；未知码按拉丁处理。
func Script(code string) string {
	if s, ok := scripts[code]; ok {
		return s
	}
	return ScriptLatin
}

// scriptOf 判定单个字符属于哪个体系；返回空串表示不参与统计。
//
// 数字、标点、空格不参与 —— 一条 SQL、一段 JSON 的体系应由其中的字母决定，
// 否则 `id` 这种带下划线的标识符会被无关字符带偏。
func scriptOf(r rune) string {
	switch {
	case r >= 0x3040 && r <= 0x30FF: // 平假名、片假名
		return ScriptKana
	case r >= 0x3130 && r <= 0x318F, r >= 0xA960 && r <= 0xA97F,
		r >= 0xAC00 && r <= 0xD7A3: // 谚文
		return ScriptHangul
	case r >= 0x3400 && r <= 0x4DBF, r >= 0x4E00 && r <= 0x9FFF,
		r >= 0xF900 && r <= 0xFAFF: // 汉字
		return ScriptHan
	case r >= 0x0400 && r <= 0x04FF:
		return ScriptCyrillic
	case r >= 0x0600 && r <= 0x06FF:
		return ScriptArabic
	case r >= 0x0590 && r <= 0x05FF:
		return ScriptHebrew
	case r >= 0x0E00 && r <= 0x0E7F:
		return ScriptThai
	case r >= 0x0900 && r <= 0x097F:
		return ScriptDevanagari
	case r >= 0x0370 && r <= 0x03FF:
		return ScriptGreek
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
		r >= 0x00C0 && r <= 0x024F: // 拉丁扩展：é ñ ü ş …
		return ScriptLatin
	default:
		return ""
	}
}

// scriptLang 把体系映射回一个代表语言码。
//
// 注意这只是「代表」：han→zh 分不清简繁，cyrillic→ru 分不清俄乌。
// 所以换向时源语言交给 auto 让引擎自己判，只有目标语言用这个映射。
var scriptLang = map[string]string{
	ScriptHan:        "zh",
	ScriptKana:       "ja",
	ScriptHangul:     "ko",
	ScriptCyrillic:   "ru",
	ScriptArabic:     "ar",
	ScriptHebrew:     "he",
	ScriptThai:       "th",
	ScriptDevanagari: "hi",
	ScriptGreek:      "el",
	ScriptLatin:      "en",
}

// Detect 判断文本的主要文字体系，返回对应的代表语言码。
//
// 门槛刻意收紧：至少 2 个可归类字符，且该体系占多数（≥一半）。
// 因为返回值会被拿去【改变翻译方向】，误判比不判更糟。
func Detect(text string) (string, bool) {
	counts := map[string]int{}
	total := 0
	for _, r := range text {
		s := scriptOf(r)
		if s == "" {
			continue
		}
		counts[s]++
		total++
	}
	if total < 2 {
		return "", false
	}
	best, bestN := "", 0
	// 固定顺序遍历，不用 map 迭代序：否则同样输入两次判定可能不同（不可重现）
	for _, s := range []string{
		ScriptHan, ScriptKana, ScriptHangul, ScriptCyrillic, ScriptArabic,
		ScriptHebrew, ScriptThai, ScriptDevanagari, ScriptGreek, ScriptLatin,
	} {
		if n := counts[s]; n > bestN {
			best, bestN = s, n
		}
	}
	if best == "" || bestN*2 < total {
		return "", false
	}
	l, ok := scriptLang[best]
	return l, ok
}

// AutoDirection 计算方向自适应后的源/目标语言。
//
// 规则：
//   - 原文语系 ≠ 目标语言语系 → 一律不动（本来就该这么译）
//   - 语系冲突（目标中文、贴进来的是中文）→ 换目标语言，并把源语言交给 auto：
//     若用户设了「源=英 目标=中」而贴进中文，只换目标会凑出「英→英」的荒唐组合，
//     传统 MT 引擎喂进去必然出错
//
// 新目标的选择：优先用用户设定的源语言（只要它与原文不同语系）；
// 否则拉丁原文换成中文、非拉丁原文换成英文（后者与 IDEA 插件行为一致）。
func AutoDirection(sourceLang, targetLang, text string) (src, dst, detected string, changed bool) {
	detected, ok := Detect(text)
	if !ok {
		return sourceLang, targetLang, "", false
	}
	if Script(detected) != Script(targetLang) {
		return sourceLang, targetLang, detected, false
	}
	next := ""
	switch {
	case sourceLang != Auto && sourceLang != "" && Script(sourceLang) != Script(detected):
		next = sourceLang
	case Script(detected) == ScriptLatin:
		next = "zh"
	default:
		next = "en"
	}
	if next == "" || next == targetLang {
		return sourceLang, targetLang, detected, false
	}
	return Auto, next, detected, true
}
