package main

// 本文件放在 main 包而非 engine 包，是因为引擎适配器通过各自子包的 init()
// 注册到 engine 的注册表里 —— 只有 main 包（用空白导入引入了全部适配器）
// 才能看到完整的注册表。放在 engine 包里测试会看到一张空表，
// 从而误报「所有引擎都没注册」。

import (
	"sort"
	"testing"

	"github.com/gaogao/translation/engine"
	"github.com/gaogao/translation/lang"
)

// TestRegistry_LangMapsMatchAdapters 验证 lang 包的映射表与 engine 注册表【完全一致】。
//
// 这两个表由不同的人在不同时间维护，极易漂移：
//   - lang 里有映射但没有适配器 → 用户能在下拉框里选到这个类型，
//     但保存时报「未知的引擎类型」，看起来像 bug
//   - 有适配器但 lang 里没映射 → SupportedLangs 返回空，
//     前端语言下拉框全被禁用，用户以为插件不支持任何语言
//
// 两种漂移都不会在编译期报错，只会在运行时表现为「某个引擎莫名其妙不能用」。
func TestRegistry_LangMapsMatchAdapters(t *testing.T) {
	reg := engine.SortKinds(engine.Kinds())
	lm := lang.KnownKinds()
	sort.Strings(lm)

	regSet := map[string]bool{}
	for _, k := range reg {
		regSet[k] = true
	}
	lmSet := map[string]bool{}
	for _, k := range lm {
		lmSet[k] = true
	}

	for _, k := range lm {
		if !regSet[k] {
			t.Errorf("lang 包有 %q 的映射表，但没有对应的引擎适配器"+
				"（用户能选到这个类型却保存不了）。请实现 backend/engine/%s/ 或从 lang.maps 移除", k, k)
		}
	}
	for _, k := range reg {
		if !lmSet[k] {
			t.Errorf("引擎 %q 已注册，但 lang 包没有它的语言映射表"+
				"（SupportedLangs 会返回空，前端语言下拉框会全部禁用）。请在 lang.maps 里补上映射", k)
		}
	}
}

// TestRegistry_NoDuplicateKinds 验证没有重复注册。
//
// Register 对重复 kind 是【覆盖】而非 panic（panic 会让侧车启动失败，
// 表现为「插件完全无响应」，排查成本远高于一个被覆盖的注册）。
// 但覆盖本身是 bug —— 两个包注册同一个 kind，后注册的静默生效，
// 前一个的所有代码变成死代码。
func TestRegistry_NoDuplicateKinds(t *testing.T) {
	kinds := engine.Kinds()
	seen := map[string]int{}
	for _, k := range kinds {
		seen[k]++
	}
	for k, n := range seen {
		if n > 1 {
			t.Errorf("kind %q 被注册了 %d 次（后者会静默覆盖前者）", k, n)
		}
	}
	if len(kinds) != len(seen) {
		t.Errorf("Kinds() 返回了 %d 项但只有 %d 个唯一 kind", len(kinds), len(seen))
	}
}

// TestRegistry_SchemasComplete 验证每个已注册引擎的 schema 都完整可用。
//
// 前端表单完全由 schema 驱动，schema 缺字段就等于「这个引擎没法配置」。
func TestRegistry_SchemasComplete(t *testing.T) {
	schemas := engine.Schemas()
	if len(schemas) != len(engine.Kinds()) {
		t.Errorf("Schemas() 返回 %d 项，Kinds() 返回 %d 项，数量应一致",
			len(schemas), len(engine.Kinds()))
	}
	for _, s := range schemas {
		if s.Kind == "" {
			t.Error("schema 的 Kind 为空（Register 应自动填入）")
		}
		if s.Label == "" {
			t.Errorf("引擎 %q 的 schema 缺少 Label（下拉框会显示空白）", s.Kind)
		}
		if s.LabelEn == "" {
			t.Errorf("引擎 %q 的 schema 缺少 LabelEn（英文界面会回退到中文）", s.Kind)
		}
		// DefaultName 是「选类型自动补全名称」功能的前提
		if s.DefaultName == "" {
			t.Errorf("引擎 %q 的 schema 缺少 DefaultName（前端无法自动补全名称）", s.Kind)
		}
		if s.DefaultNameEn == "" {
			t.Errorf("引擎 %q 的 schema 缺少 DefaultNameEn", s.Kind)
		}
		if s.MaxChars <= 0 {
			t.Errorf("引擎 %q 的 MaxChars = %d，必须为正数（分段器依赖它）", s.Kind, s.MaxChars)
		}
		if len(s.Fields) == 0 {
			t.Errorf("引擎 %q 的 schema 没有任何字段（表单会是空的）", s.Kind)
		}

		secretFields := 0
		keys := map[string]bool{}
		for _, f := range s.Fields {
			if f.Key == "" || f.Label == "" || f.Type == "" {
				t.Errorf("引擎 %q 的字段定义不完整: %+v", s.Kind, f)
			}
			if keys[f.Key] {
				t.Errorf("引擎 %q 的字段 key %q 重复（后者会覆盖前者，配置丢失）", s.Kind, f.Key)
			}
			keys[f.Key] = true
			if (f.Type == engine.FieldSelect || f.Type == engine.FieldRadio) && len(f.Options) == 0 {
				t.Errorf("引擎 %q 的字段 %s 是 %s 但没有 options（下拉框会是空的）",
					s.Kind, f.Key, f.Type)
			}
			if f.Secret {
				secretFields++
				if f.Type != engine.FieldPassword {
					t.Errorf("引擎 %q 的 Secret 字段 %s 类型应是 password，得到 %s"+
						"（否则前端不会掩码显示）", s.Kind, f.Key, f.Type)
				}
			}
		}
		if s.NeedsSecret && secretFields == 0 {
			t.Errorf("引擎 %q 声明 NeedsSecret 但没有任何 Secret 字段", s.Kind)
		}
		if !s.NeedsSecret && secretFields > 0 {
			t.Errorf("引擎 %q 有 Secret 字段但 NeedsSecret=false（前端不会提示必填）", s.Kind)
		}
	}
}

// TestRegistry_LanguagesNonEmpty 验证每个引擎都能报告支持的语言。
//
// 前端据此禁用下拉框里不支持的选项。返回空列表会让所有语言都被禁用，
// 用户看到的现象是「一个语言都选不了」。
func TestRegistry_LanguagesNonEmpty(t *testing.T) {
	for _, kind := range engine.Kinds() {
		langs := lang.SupportedLangs(kind)
		if len(langs) == 0 {
			t.Errorf("引擎 %q 的 SupportedLangs 为空（前端语言下拉框会全部禁用）", kind)
			continue
		}
		targets := lang.SupportedTargetLangs(kind)
		if len(targets) == 0 {
			t.Errorf("引擎 %q 的 SupportedTargetLangs 为空", kind)
		}
		// 目标语言列表不应含 auto（翻译成「自动检测」没有意义）
		for _, c := range targets {
			if c == lang.Auto {
				t.Errorf("引擎 %q 的目标语言列表不应含 auto", kind)
			}
		}
		// 中文与英语是所有引擎的最低要求
		for _, need := range []string{"zh", "en"} {
			if !lang.Supports(kind, need) {
				t.Errorf("引擎 %q 应至少支持 %s", kind, need)
			}
		}
	}
}

// TestBuild_UnknownKind 验证构造未知类型时返回可理解的错误。
func TestBuild_UnknownKind(t *testing.T) {
	_, err := engine.Build("不存在的引擎", nil, nil, engine.Options{})
	if err == nil {
		t.Fatal("未知 kind 应报错")
	}
	e := engine.Classify(err)
	if e.Kind != engine.KindConfig {
		t.Errorf("未知 kind 应归类为 KindConfig，得到 %s", e.KindName())
	}
	if e.Code() != -32602 {
		t.Errorf("错误码 = %d, 期望 -32602", e.Code())
	}
}

// TestClassify_NilSafe 验证 Classify(nil) 不 panic。
func TestClassify_NilSafe(t *testing.T) {
	if got := engine.Classify(nil); got != nil {
		t.Errorf("Classify(nil) 应返回 nil，得到 %v", got)
	}
}
