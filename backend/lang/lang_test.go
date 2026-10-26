package lang

import (
	"testing"
)

// TestCanonical_NoDuplicates 验证规范语言码无重复。
// 重复会导致映射表静默覆盖，UI 下拉框出现两个同名选项。
func TestCanonical_NoDuplicates(t *testing.T) {
	seen := map[string]bool{}
	for _, l := range canonical {
		if seen[l.Code] {
			t.Errorf("规范语言码重复: %s", l.Code)
		}
		seen[l.Code] = true
		if l.Zh == "" || l.En == "" || l.LLM == "" {
			t.Errorf("语言 %s 的名称字段有空值: zh=%q en=%q llm=%q", l.Code, l.Zh, l.En, l.LLM)
		}
	}
	// 计划规定 30 种语言 + auto = 31
	if len(canonical) != 31 {
		t.Errorf("规范语言数量 = %d, 期望 31（30 种 + auto）", len(canonical))
	}
}

// TestAll_ReturnsCopy 验证 All() 返回副本，调用方修改不影响包内状态。
func TestAll_ReturnsCopy(t *testing.T) {
	a := All()
	a[0].Code = "被篡改"
	b := All()
	if b[0].Code == "被篡改" {
		t.Error("All() 返回了内部切片，调用方修改污染了包内状态")
	}
}

// TestMap_KnownVectors 用已知向量断言各引擎的映射正确性。
// 这些是各引擎官方文档公布的码，写错会导致翻译请求被上游拒绝。
func TestMap_KnownVectors(t *testing.T) {
	cases := []struct {
		kind, code, want string
	}{
		// DeepL：大写码，英语/葡萄牙语带地区后缀，挪威语是 NB
		{"deepl", "zh", "ZH"},
		{"deepl", "en", "EN-US"},
		{"deepl", "pt", "PT-PT"},
		{"deepl", "no", "NB"},
		{"deepl", "ja", "JA"},
		// 百度：日语 jp、韩语 kor、繁体 cht、法语 fra、西班牙语 spa
		{"baidu", "ja", "jp"},
		{"baidu", "ko", "kor"},
		{"baidu", "zh-TW", "cht"},
		{"baidu", "fr", "fra"},
		{"baidu", "es", "spa"},
		{"baidu", "he", "heb"},
		{"baidu", "sv", "swe"},
		// 有道：简体 zh-CHS、繁体 zh-CHT
		{"youdao", "zh", "zh-CHS"},
		{"youdao", "zh-TW", "zh-CHT"},
		{"youdao", "en", "en"},
		// 阿里：小写，繁体 zh-tw
		{"aliyun", "zh", "zh"},
		{"aliyun", "zh-TW", "zh-tw"},
		{"aliyun", "en", "en"},
		// 大模型类：映射到自然语言名
		{"openai", "zh", "Simplified Chinese"},
		{"openai", "en", "English"},
		{"localcli", "ja", "Japanese"},
	}
	for _, c := range cases {
		got, ok := Map(c.kind, c.code)
		if !ok {
			t.Errorf("Map(%q, %q) 返回 ok=false，期望 %q", c.kind, c.code, c.want)
			continue
		}
		if got != c.want {
			t.Errorf("Map(%q, %q) = %q, 期望 %q", c.kind, c.code, got, c.want)
		}
	}
}

// TestMap_Unsupported 验证不支持的组合返回 ok=false。
// 这是 UI 禁用下拉项的依据，返回 true 会导致用户选了之后翻译失败。
func TestMap_Unsupported(t *testing.T) {
	cases := []struct{ kind, code string }{
		{"deepl", "zh-TW"}, // DeepL 只有 ZH，不区分简繁
		{"deepl", "ms"},    // DeepL 不支持马来语
		{"youdao", Auto},   // 有道不支持自动检测
		{"openai", "不存在的码"},
		{"deepl", "不存在的码"},
	}
	for _, c := range cases {
		if got, ok := Map(c.kind, c.code); ok {
			t.Errorf("Map(%q, %q) 应返回 ok=false，却得到 %q", c.kind, c.code, got)
		}
	}
}

// TestMap_UnknownKind 验证未注册的引擎 kind 返回 ok=false 而不是 panic。
func TestMap_UnknownKind(t *testing.T) {
	if _, ok := Map("不存在的引擎", "zh"); ok {
		t.Error("未注册 kind 应返回 ok=false")
	}
	if _, ok := Map("", "zh"); ok {
		t.Error("空 kind 应返回 ok=false")
	}
}

// TestMapTarget_RejectsAuto 验证目标语言位置拒绝 auto。
// 把文本翻译成"自动检测"没有意义，这个检查集中在 lang 包避免六个引擎各写一遍。
func TestMapTarget_RejectsAuto(t *testing.T) {
	for _, kind := range KnownKinds() {
		if got, ok := MapTarget(kind, Auto); ok {
			t.Errorf("MapTarget(%q, auto) 应返回 ok=false，却得到 %q", kind, got)
		}
	}
}

// TestMapSource_AllowsAutoWhereSupported 验证源语言位置的 auto 支持情况。
func TestMapSource_AllowsAutoWhereSupported(t *testing.T) {
	supports := map[string]bool{
		"openai": true, "localcli": true, "deepl": true,
		"baidu": true, "aliyun": true,
		"youdao": false, // 有道 API 要求显式 from 语言
	}
	for kind, want := range supports {
		got := SupportsAutoSource(kind)
		if got != want {
			t.Errorf("SupportsAutoSource(%q) = %v, 期望 %v", kind, got, want)
		}
	}
}

// TestSupportedLangs 验证支持列表的内容与顺序。
func TestSupportedLangs(t *testing.T) {
	// 大模型类应支持全部 31 个（含 auto）
	for _, kind := range []string{"openai", "localcli"} {
		got := SupportedLangs(kind)
		if len(got) != 31 {
			t.Errorf("SupportedLangs(%q) 长度 = %d, 期望 31", kind, len(got))
		}
		if got[0] != Auto {
			t.Errorf("SupportedLangs(%q)[0] = %q, 期望 auto（应置顶）", kind, got[0])
		}
	}

	// 有道不含 auto
	yd := SupportedLangs("youdao")
	for _, c := range yd {
		if c == Auto {
			t.Error("有道不应支持 auto")
		}
	}
	if len(yd) != 30 {
		t.Errorf("SupportedLangs(youdao) 长度 = %d, 期望 30", len(yd))
	}

	// DeepL 缺 zh-TW 与 ms，但含 auto → 31 - 2 = 29
	dl := SupportedLangs("deepl")
	if len(dl) != 29 {
		t.Errorf("SupportedLangs(deepl) 长度 = %d, 期望 29", len(dl))
	}

	// 未注册引擎返回空切片而非 nil（前端可直接遍历）
	unknown := SupportedLangs("不存在")
	if unknown == nil {
		t.Error("未注册引擎应返回空切片而非 nil")
	}
	if len(unknown) != 0 {
		t.Errorf("未注册引擎应返回空切片，得到 %d 项", len(unknown))
	}
}

// TestSupportedTargetLangs_ExcludesAuto 验证目标语言列表不含 auto。
func TestSupportedTargetLangs_ExcludesAuto(t *testing.T) {
	for _, kind := range KnownKinds() {
		for _, c := range SupportedTargetLangs(kind) {
			if c == Auto {
				t.Errorf("SupportedTargetLangs(%q) 不应含 auto", kind)
			}
		}
	}
}

// TestSupportedLangs_OrderStable 验证顺序与 canonical 一致（UI 下拉框顺序稳定）。
func TestSupportedLangs_OrderStable(t *testing.T) {
	got := SupportedLangs("openai")
	for i, l := range canonical {
		if got[i] != l.Code {
			t.Errorf("第 %d 项 = %q, 期望 %q（顺序应与 canonical 一致）", i, got[i], l.Code)
		}
	}
}

// TestNames_Fallback 验证未知码的名称查询回退为码本身而非报错。
func TestNames_Fallback(t *testing.T) {
	if got := LLMName("xx-YY"); got != "xx-YY" {
		t.Errorf("LLMName 未知码应回退为码本身，得到 %q", got)
	}
	if got := ZhName("xx-YY"); got != "xx-YY" {
		t.Errorf("ZhName 未知码应回退为码本身，得到 %q", got)
	}
	if got := EnName("xx-YY"); got != "xx-YY" {
		t.Errorf("EnName 未知码应回退为码本身，得到 %q", got)
	}
	// 已知码应返回真实名称
	if got := ZhName("zh"); got != "简体中文" {
		t.Errorf("ZhName(zh) = %q, 期望 简体中文", got)
	}
	if got := LLMName("zh"); got != "Simplified Chinese" {
		t.Errorf("LLMName(zh) = %q, 期望 Simplified Chinese", got)
	}
}

// TestIsValid 验证规范码校验。
func TestIsValid(t *testing.T) {
	if !IsValid(Auto) {
		t.Error("auto 应为合法码")
	}
	if !IsValid("zh") || !IsValid("zh-TW") {
		t.Error("zh / zh-TW 应为合法码")
	}
	if IsValid("") || IsValid("不存在") || IsValid("ZH") {
		t.Error("空串/未知码/大小写不符 都不应合法")
	}
}

// TestKnownKinds 验证已注册引擎清单完整。
func TestKnownKinds(t *testing.T) {
	// 8 类引擎：大模型类 2 个（openai / localcli）+ 传统机翻 6 个
	want := map[string]bool{
		"openai": true, "localcli": true, "deepl": true, "google": true,
		"baidu": true, "youdao": true, "aliyun": true, "tencent": true,
	}
	got := KnownKinds()
	if len(got) != len(want) {
		t.Errorf("KnownKinds() 长度 = %d, 期望 %d: %v", len(got), len(want), got)
	}
	for _, k := range got {
		if !want[k] {
			t.Errorf("KnownKinds() 含未预期的 kind: %s", k)
		}
	}
}

// autoMeansOmit 列出「用空串表示自动检测」的引擎。
//
// 这些引擎的 API 约定是【省略源语言参数】而非传一个特殊值：
//   - DeepL：不传 source_lang 即自动检测（传空串会 400）
//   - Google：不传 source 即自动检测
//
// 因此它们的 auto 映射为空串是【有意设计】，不是漏填。
// 对应的适配器必须检查「映射结果非空才设置参数」——
// google.go 与 deepl.go 都有 `if src != ""` 这道判断，
// 各自的单元测试覆盖了「auto 时请求体不含 source 字段」。
var autoMeansOmit = map[string]bool{"deepl": true, "google": true}

// TestMaps_AllValuesAreValidEngineCodes 验证映射表的值都非空。
//
// 空值会让上游收到空语言参数，报错信息往往很难懂。
// 唯一例外是 autoMeansOmit 里那些引擎的 auto 键（见上方说明）。
func TestMaps_AllValuesAreValidEngineCodes(t *testing.T) {
	for kind, m := range maps {
		for code, v := range m {
			if v == "" && !(code == Auto && autoMeansOmit[kind]) {
				t.Errorf("映射表 %s[%q] 为空值（若该引擎确实用「省略参数」表示自动检测，"+
					"请把它加进 autoMeansOmit，并确认适配器有「映射结果非空才设置参数」的判断）", kind, code)
			}
		}
	}
}

// TestMaps_AutoOmitEnginesHaveEmptyAuto 反向验证：
// autoMeansOmit 里声明的引擎，其 auto 映射【必须】是空串。
//
// 这条与上一条互补，防止有人改了映射表却忘了同步 autoMeansOmit，
// 或反过来 —— 两边不一致时，要么测试误报，要么真正的空值漏检。
func TestMaps_AutoOmitEnginesHaveEmptyAuto(t *testing.T) {
	for kind := range autoMeansOmit {
		m, ok := maps[kind]
		if !ok {
			t.Errorf("autoMeansOmit 声明了 %q，但 maps 里没有这个引擎", kind)
			continue
		}
		v, exists := m[Auto]
		if !exists {
			t.Errorf("引擎 %q 用省略参数表示自动检测，但映射表里没有 %s 键", kind, Auto)
			continue
		}
		if v != "" {
			t.Errorf("引擎 %q 在 autoMeansOmit 里，但 %s 映射为 %q 而非空串", kind, Auto, v)
		}
	}
}
