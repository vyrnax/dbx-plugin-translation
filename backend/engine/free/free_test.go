package free

import (
	"errors"
	"strings"
	"testing"

	"github.com/vyrnax/translation/engine"
	"github.com/vyrnax/translation/lang"
)

// TestSeedConfig 逐项钉住内置引擎的锁定配置。
//
// 这些值全部来自 2026-10-26 的实测（见包注释），不是拍脑袋：
//   - 单段 400 字符：2B 在 410 字符 1136ms 成功、820 字符 HTTP 504
//   - 超时 20 秒：必须小于服务端 30 秒网关硬超时
//   - 并发 4：实测无限流，但全局在飞任务上限也是 4，再高只会互相争抢
//
// 断言值来自「实测规范」而不是「实现现在输出什么」（AGENTS.md 6.8）。
func TestSeedConfig(t *testing.T) {
	cfg := SeedConfig()
	checks := []struct {
		key  string
		want any
	}{
		{"endpoint", Endpoint},
		{"model", Model},
		{"maxChars", SafeMaxChars},
		{"timeoutSecs", TimeoutSecs},
		{"concurrency", Concurrency},
		{"temperature", 0},
		{"noAuth", true},
	}
	for _, c := range checks {
		if got := cfg[c.key]; got != c.want {
			t.Errorf("SeedConfig[%q] = %v, 期望 %v", c.key, got, c.want)
		}
	}
	if !strings.HasPrefix(Endpoint, "https://") {
		t.Errorf("内置端点必须是 HTTPS（明文 HTTP 会把译文暴露在链路上）：%s", Endpoint)
	}
	if TimeoutSecs >= 30 {
		t.Errorf("timeoutSecs = %d，必须小于服务端 30 秒网关超时，否则 504 永远抢不到话", TimeoutSecs)
	}
	if SafeMaxChars > 500 {
		t.Errorf("SafeMaxChars = %d，实测 820 字符就 504，上限不该高于 500", SafeMaxChars)
	}
}

// TestKindIsNotOpenai 验证 kind 被改回 "free"。
//
// 这是最容易被漏掉的一处：内层复用 openai 适配器，Kind() 默认返回 "openai"。
// 而 kind 同时是【并发信号量的 key】与【语言映射表的索引】，
// 不改的话内置引擎会和用户自配的 OpenAI 兼容引擎抢同一个信号量，
// 并拿错语言支持集 —— 症状是「配了第二个引擎后免费引擎莫名变慢」。
func TestKindIsNotOpenai(t *testing.T) {
	e, err := engine.Build(Kind, nil, nil, engine.Options{Name: NameZh})
	if err != nil {
		t.Fatalf("内置引擎构造失败: %v", err)
	}
	if got := e.Kind(); got != Kind {
		t.Errorf("Kind() = %q, 期望 %q（内层适配器的值泄漏出来了）", got, Kind)
	}
	if got := e.MaxChars(); got != SafeMaxChars {
		t.Errorf("MaxChars() = %d, 期望 %d", got, SafeMaxChars)
	}
	if !e.SupportsStreaming() {
		t.Error("该服务支持 SSE 流式，SupportsStreaming 应为 true（否则即时翻译会退成整段返回）")
	}
	langs := e.SupportedLangs()
	if len(langs) == 0 {
		t.Fatal("SupportedLangs 为空（前端语言下拉会全部禁用）")
	}
	if _, ok := lang.Map(Kind, "en"); !ok {
		t.Error("free 缺少 en 的语言映射")
	}
}

// TestBuildIgnoresCallerConfig 验证调用方传不进任何配置。
//
// 内置引擎「不可编辑」必须由代码保证，而不是只靠 UI 不显示编辑按钮：
// 只要 engines/save 能被构造出一条指向内置 id 的请求，
// 这里就必须让传进来的 endpoint/maxChars 完全无效。
// maxChars 是最容易观察的证据（不联网也能断言），所以用它做探针。
func TestBuildIgnoresCallerConfig(t *testing.T) {
	e, err := engine.Build(Kind,
		engine.Config{"endpoint": "https://evil.example.com/v1/chat/completions", "maxChars": 99999},
		engine.Secrets{"apiKey": "sk-should-be-ignored"},
		engine.Options{})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if got := e.MaxChars(); got != SafeMaxChars {
		t.Errorf("调用方的 maxChars 生效了：MaxChars() = %d, 期望恒为 %d", got, SafeMaxChars)
	}
}

// TestFriendlyError504 验证「30 秒网关超时」被改写成可操作的建议。
func TestFriendlyError504(t *testing.T) {
	in := engine.New(engine.KindUpstream, "网关超时（504）：上游服务响应过慢")
	out := friendlyError(in)
	oe := engine.Classify(out)
	if oe == nil {
		t.Fatal("friendlyError 丢了错误类型")
	}
	if !strings.Contains(oe.Message, "30 秒") {
		t.Errorf("改写后的消息应说明 30 秒网关超时: %q", oe.Message)
	}
	if !strings.Contains(oe.Detail, "官方引擎") {
		t.Errorf("应给出下一步（换官方引擎），而不是只描述现象: %q", oe.Detail)
	}
}

// TestFriendlyErrorKeepsOthers 验证【只有】确知的 504 会被改写。
//
// 反过来必须证明：不能把别的错误也改写掉。否则服务商真实原因
// （配额耗尽、模型下线、签名失效）会被我们的建议盖住，那是更难排查的问题。
func TestFriendlyErrorKeepsOthers(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"鉴权失败", engine.New(engine.KindAuth, "鉴权失败（401）：API Key 无效或已过期")},
		{"上游 500", engine.New(engine.KindUpstream, "服务端内部错误（500）")},
		{"普通取消", engine.Wrap(engine.KindCancelled, "翻译已取消", errors.New("canceled"))},
		{"数字巧合", engine.New(engine.KindUpstream, "请求失败：第 504 项不存在")},
	}
	for _, c := range cases {
		got := friendlyError(c.err)
		if engine.Classify(got).Message != engine.Classify(c.err).Message {
			t.Errorf("%s 的错误消息被改写了（应当原样透传）: %q", c.name, engine.Classify(got).Message)
		}
	}
	if friendlyError(nil) != nil {
		t.Error("nil 必须原样返回 nil")
	}
}

// TestSchemaBuiltinContract 验证 schema 上的内置契约。
//
// 前端完全依赖这三个标记来决定「隐藏编辑/删除」「从类型下拉里排除」，
// 少写一个就是一个静默的 UI 缺陷（内置引擎被当成普通引擎让用户改端点）。
func TestSchemaBuiltinContract(t *testing.T) {
	_, sch, ok := engine.Lookup(Kind)
	if !ok {
		t.Fatal("free 没注册进引擎工厂")
	}
	if !sch.Builtin {
		t.Error("Schema.Builtin 必须为 true")
	}
	if !sch.NoThrottle {
		t.Error("Schema.NoThrottle 必须为 true（实测该端点无限流，不该吃 1100ms 段间延时）")
	}
	if sch.NeedsSecret {
		t.Error("内置引擎不能要求密钥：它没有任何可填字段")
	}
	if len(sch.Fields) != 0 {
		t.Errorf("内置引擎应当零字段，实际 %d 个", len(sch.Fields))
	}
	if sch.Icon != "default.svg" {
		t.Errorf("Icon = %q, 期望 default.svg（内置引擎没有品牌 logo，用通用图标）", sch.Icon)
	}
	if sch.MaxChars != SafeMaxChars {
		t.Errorf("Schema.MaxChars = %d，必须与适配器一致（前端表单提示与分段器都读它）", sch.MaxChars)
	}
	if sch.Description == "" || !strings.Contains(sch.Description, "公益") {
		t.Error("说明文案必须写清这是公益端点（用户需要知道它不保证可用）")
	}
}

// TestNameMatchesSchema 验证展示名与 schema 不打架。
func TestNameMatchesSchema(t *testing.T) {
	_, sch, _ := engine.Lookup(Kind)
	if sch.Label != NameZh || sch.LabelEn != NameEn {
		t.Errorf("schema 名字 %q/%q 与常量 %q/%q 不一致", sch.Label, sch.LabelEn, NameZh, NameEn)
	}
	if sch.DefaultName != NameZh || sch.DefaultNameEn != NameEn {
		t.Error("DefaultName 必须等于展示名，否则存储记录与 UI 显示会各说一套")
	}
}
