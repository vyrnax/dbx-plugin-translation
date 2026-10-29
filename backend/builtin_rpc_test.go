package main

import (
	"strings"
	"testing"

	"github.com/vyrnax/translation/engine"
	"github.com/vyrnax/translation/engine/free"
	"github.com/vyrnax/translation/store"
)

// 本文件覆盖「内置免费引擎」在 RPC 层的契约：
// 不可删除、不可改写、可以禁用、可以设默认、schema 与 list 要把内置标记带给前端。
//
// 注意 Handle 返回的是 Go 原生结构而不是 JSON —— 嵌套数组的实际类型是
// []map[string]any，断言成 []any 会静默拿到 nil，表现为「列表是空的」这种假故障。
// 现有测试一律用具体类型断言，本文件沿用同一写法。

// idsOf 把 engines/list 的 engines 字段拼成 "a,b,c"，用于比较顺序。
// 注意 Handle 返回的是 Go 原生结构，元素类型是 map[string]any 而不是 any
// —— 断言成 []any 会静默拿到 nil，看起来像「接口返回空列表」（AGENTS.md 6.8）。
func idsOf(v any) string {
	list, _ := v.([]map[string]any)
	ids := make([]string, 0, len(list))
	for _, m := range list {
		id, _ := m["id"].(string)
		ids = append(ids, id)
	}
	return strings.Join(ids, ",")
}

// seedBuiltin 在测试插件上复现启动流程里的内置引擎播种。
//
// 为什么不让 newTestPlugin 直接播种：那会让既有的几十个 RPC 测试
// 无缘无故多出一条引擎记录，「列表数量 = 1」这类断言全部要跟着改，
// 而且改完之后测试就不再表达它原本的意图。内置行为单独成组测试，边界更清楚。
func seedBuiltin(t *testing.T, p *plugin) {
	t.Helper()
	p.builtinTpl = store.Engine{
		ID:      store.BuiltinEngineID,
		Name:    free.NameZh,
		Kind:    free.Kind,
		Config:  free.SeedConfig(),
		Enabled: true,
		Builtin: true,
	}
	if _, err := p.store.EnsureBuiltin(p.builtinTpl); err != nil {
		t.Fatalf("播种内置引擎失败: %v", err)
	}
}

// TestBuiltin_RPC_ListFlagsIt 验证 engines/list 把内置标记传给前端。
//
// 前端「隐藏编辑/删除」「从类型下拉里排除」完全依赖这个字段；
// RPC 层漏一行（engineToJSON 是手工挑字段组装 map 的）就是一个静默的 UI 缺陷。
func TestBuiltin_RPC_ListFlagsIt(t *testing.T) {
	p := newTestPlugin(t)
	seedBuiltin(t, p)

	res, rpcErr := call(p, "engines/list", map[string]any{})
	if rpcErr != nil {
		t.Fatalf("engines/list 失败: %v", rpcErr)
	}
	list, ok := res["engines"].([]map[string]any)
	if !ok {
		t.Fatalf("engines 类型不符期望 []map[string]any，实际 %T", res["engines"])
	}
	if len(list) != 1 {
		t.Fatalf("引擎数量 = %d, 期望 1", len(list))
	}
	row := list[0]
	if row["builtin"] != true {
		t.Errorf("engines/list 未返回 builtin=true: %v", row)
	}
	if row["kind"] != free.Kind {
		t.Errorf("kind = %v, 期望 %s", row["kind"], free.Kind)
	}
	// 内置引擎没有密钥：hasSecret 必须为 false，否则 UI 会显示「未配置密钥」
	if row["hasSecret"] == true {
		t.Error("内置引擎不应带 hasSecret")
	}
}

// TestBuiltin_RPC_SchemaFlagsIt 验证 translate/schema 下发 builtin / noThrottle。
func TestBuiltin_RPC_SchemaFlagsIt(t *testing.T) {
	p := newTestPlugin(t)
	res, rpcErr := call(p, "translate/schema", map[string]any{})
	if rpcErr != nil {
		t.Fatalf("translate/schema 失败: %v", rpcErr)
	}
	kinds, ok := res["kinds"].([]map[string]any)
	if !ok {
		t.Fatalf("kinds 类型不符期望 []map[string]any，实际 %T", res["kinds"])
	}
	var found bool
	for _, m := range kinds {
		if m["kind"] != free.Kind {
			continue
		}
		found = true
		if m["builtin"] != true {
			t.Errorf("schema 未标记 builtin=true: %v", m)
		}
		if m["noThrottle"] != true {
			t.Errorf("schema 未标记 noThrottle=true: %v", m)
		}
		if m["needsSecret"] == true {
			t.Error("内置引擎不该声明需要密钥（它没有可填字段）")
		}
		if icon, _ := m["icon"].(string); icon != "default.svg" {
			t.Errorf("icon = %q, 期望 default.svg", icon)
		}
	}
	if !found {
		t.Fatalf("translate/schema 的 kinds 里没有 %s（共 %d 类）", free.Kind, len(kinds))
	}
}

// TestBuiltin_RPC_DeleteRejected 验证删除内置引擎返回可操作的文案。
//
// 「不可删除」必须伴随「可以禁用」—— 只说不能做什么，
// 用户会以为插件在耍人，转而去找更暴力的办法（删数据目录）。
func TestBuiltin_RPC_DeleteRejected(t *testing.T) {
	p := newTestPlugin(t)
	seedBuiltin(t, p)

	_, rpcErr := call(p, "engines/delete", map[string]any{"id": store.BuiltinEngineID})
	if rpcErr == nil {
		t.Fatal("删除内置引擎应当失败")
	}
	if rpcErr.Code != -32602 {
		t.Errorf("错误码 = %d, 期望 -32602", rpcErr.Code)
	}
	if !strings.Contains(rpcErr.Message, "内置") || !strings.Contains(rpcErr.Message, "禁用") {
		t.Errorf("文案应当既说明不可删除、又给出可操作的替代方案: %q", rpcErr.Message)
	}
	// 被拒绝的删除不能留下副作用
	if _, err := p.store.GetEngine(store.BuiltinEngineID); err != nil {
		t.Errorf("内置引擎被删掉了: %v", err)
	}
}

// TestBuiltin_RPC_SaveCannotRewrite 验证绕过 UI 直接调 engines/save 也改不动内置记录。
//
// 这是【后端】防线，防的是 data/import、手工构造的 RPC、
// 以及未来任何新增写入路径 —— 只靠前端不显示按钮，等于没有防线。
func TestBuiltin_RPC_SaveCannotRewrite(t *testing.T) {
	p := newTestPlugin(t)
	seedBuiltin(t, p)

	_, rpcErr := call(p, "engines/save", map[string]any{
		"id":     store.BuiltinEngineID,
		"kind":   "openai",
		"name":   "我改的内置引擎",
		"config": map[string]any{"endpoint": "https://evil.example.com/v1/chat/completions"},
		"rev":    -1,
	})
	if rpcErr != nil {
		t.Fatalf("engines/save 失败: %v", rpcErr)
	}
	e, err := p.store.GetEngine(store.BuiltinEngineID)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if e.Name != free.NameZh {
		t.Errorf("名字被改写: %q, 期望 %q", e.Name, free.NameZh)
	}
	if e.Kind != free.Kind {
		t.Errorf("类型被改写: %q", e.Kind)
	}
	if got, _ := e.Config["endpoint"].(string); got != free.Endpoint {
		t.Errorf("端点被改写: %q, 期望 %q", got, free.Endpoint)
	}
	if !e.Builtin {
		t.Error("builtin 标记被抹掉了（删除保护会随之失效）")
	}
}

// TestBuiltin_RPC_ToggleAndDefaultAllowed 验证内置引擎支持禁用与设默认。
//
// 用户要的「不用它」是这两条路径，不是删除。必须证明它们可用 ——
// 否则保护逻辑写过头，用户连禁用都点不动。
func TestBuiltin_RPC_ToggleAndDefaultAllowed(t *testing.T) {
	p := newTestPlugin(t)
	seedBuiltin(t, p)

	// 再建一个用户引擎，制造「切换默认」的真实场景。
	// 新建必须【不给 id】：给了 id 就走更新分支，会报「引擎不存在」。
	resSave, rpcErr := call(p, "engines/save", map[string]any{
		"kind":    "openai",
		"name":    "我的引擎",
		"config":  map[string]any{"endpoint": "https://api.example.com/v1", "model": "m"},
		"secrets": map[string]any{"apiKey": "sk-test-1234567890"},
		"rev":     -1,
	})
	if rpcErr != nil {
		t.Fatalf("保存用户引擎失败: %v", rpcErr)
	}
	userID, _ := resSave["id"].(string)
	if userID == "" {
		t.Fatal("engines/save 未返回新引擎 id")
	}

	// 首个用户引擎应当自动接管默认（否则用户配了官方引擎却还在走免费端点）
	if def, err := p.store.DefaultEngine(); err != nil || def.ID != userID {
		t.Fatalf("默认引擎未切到首个用户引擎: %+v err=%v", def, err)
	}

	if _, rpcErr := call(p, "engines/toggle", map[string]any{
		"id": store.BuiltinEngineID, "enabled": false,
	}); rpcErr != nil {
		t.Fatalf("禁用内置引擎失败: %v", rpcErr)
	}
	if e, _ := p.store.GetEngine(store.BuiltinEngineID); e.Enabled {
		t.Error("禁用未生效")
	}

	// 记下当前顺序：核心断言是「设默认【不】改变顺序」
	resBefore, rpcErr := call(p, "engines/list", map[string]any{})
	if rpcErr != nil {
		t.Fatalf("engines/list 失败: %v", rpcErr)
	}
	orderBefore := idsOf(resBefore["engines"])

	if _, rpcErr := call(p, "engines/setDefault", map[string]any{"id": store.BuiltinEngineID}); rpcErr != nil {
		t.Fatalf("设默认失败: %v", rpcErr)
	}
	listRes, rpcErr := call(p, "engines/list", map[string]any{})
	if rpcErr != nil {
		t.Fatalf("engines/list 失败: %v", rpcErr)
	}
	list, ok := listRes["engines"].([]map[string]any)
	if !ok || len(list) != 2 {
		t.Fatalf("engines/list 结构异常: %T 数量=%d", listRes["engines"], len(list))
	}
	// 顺序必须与设默认之前完全一致 —— 「设为默认」只改标记，不插队。
	// （idsOf 收的是 engines 字段本身；传整张响应 map 会得到空串，
	//  而空串 == 空串 会让断言看起来「通过」，这是必须警惕的假绿。）
	if got := idsOf(listRes["engines"]); got != orderBefore {
		t.Errorf("设默认改变了列表顺序: %s → %s", orderBefore, got)
	}

	// 「标记为默认」与「生效的默认引擎」是两件事，必须分开断言：
	// 此刻内置引擎刚被上面禁用过，而 DefaultEngine() 会【跳过被禁用的默认引擎】
	// 落到第一个启用的引擎 —— 这是刻意设计，否则用户会撞上
	// 「默认引擎被禁用 → 翻译直接报错」，而不是「自动换一台能用的」。
	var builtinRow map[string]any
	for _, m := range list {
		if m["id"] == store.BuiltinEngineID {
			builtinRow = m
		}
	}
	if builtinRow == nil || builtinRow["isDefault"] != true {
		t.Errorf("默认标记没落在内置引擎这一行: %+v", builtinRow)
	}
	if def, err := p.store.DefaultEngine(); err != nil || def.ID != userID {
		t.Errorf("被禁用的默认引擎应当被跳过、回退到启用中的用户引擎，实际 %+v err=%v", def, err)
	}

	// 重新启用内置引擎后，它才成为真正生效的默认
	if _, rpcErr := call(p, "engines/toggle", map[string]any{
		"id": store.BuiltinEngineID, "enabled": true,
	}); rpcErr != nil {
		t.Fatalf("重新启用内置引擎失败: %v", rpcErr)
	}
	if def, err := p.store.DefaultEngine(); err != nil || def.ID != store.BuiltinEngineID {
		t.Errorf("启用后生效默认应为内置引擎: %+v err=%v", def, err)
	}
	if _, err := p.store.GetEngine(userID); err != nil {
		t.Errorf("设内置为默认后用户引擎丢失: %v", err)
	}
}

// TestEffectiveChunkDelay 验证无限流引擎不吃全局段间延时。
//
// chunkDelayMs 默认 1100ms 是为百度/有道免费档的 1 QPS 设计的；
// 内置免费引擎单段只有 400 字符、分段数天然更多，
// 若照吃这个延时，长文翻译会被白白拖慢好几秒。
func TestEffectiveChunkDelay(t *testing.T) {
	p := newTestPlugin(t)
	if got := p.effectiveChunkDelay(free.Kind, 1100); got != 0 {
		t.Errorf("内置免费引擎的分段延时 = %d, 期望 0（实测上游无限流）", got)
	}
	if got := p.effectiveChunkDelay("baidu", 1100); got != 1100 {
		t.Errorf("百度免费档的分段延时 = %d, 期望沿用 1100（1 QPS）", got)
	}
	// 未知 kind 不崩溃、按原值返回（schema 查不到时保持保守行为）
	if got := p.effectiveChunkDelay("不存在的引擎", 900); got != 900 {
		t.Errorf("未知 kind 的分段延时 = %d, 期望原样返回 900", got)
	}
}

// TestBuiltin_RPC_DataImportKeepsIt 验证 replace 导入后内置引擎依然在。
//
// 旧备份文件可能根本不含这条记录（它是更早版本导出的），
// 导入完必须再兜一次底，否则「开箱可用」会在用户还原备份后失效。
func TestBuiltin_RPC_DataImportKeepsIt(t *testing.T) {
	p := newTestPlugin(t)
	seedBuiltin(t, p)

	payload := map[string]any{
		"format":  "dbx-translation-export",
		"version": 1,
		"engines": []any{map[string]any{
			"id": "old1", "kind": "openai", "name": "旧引擎",
			"config": map[string]any{"endpoint": "https://api.example.com/v1", "model": "m"},
		}},
		"history": []any{},
		"secrets": map[string]any{},
		"prefs":   map[string]any{},
	}
	res, rpcErr := call(p, "data/import", map[string]any{
		"dataBase64": mustB64(mustJSON(payload)),
		"mode":       "replace",
	})
	if rpcErr != nil {
		t.Fatalf("data/import 失败: %v", rpcErr)
	}
	if res["ok"] != true {
		t.Fatalf("导入未成功: %+v", res)
	}
	if _, err := p.store.GetEngine(store.BuiltinEngineID); err != nil {
		t.Errorf("replace 导入后内置引擎不见了: %v", err)
	}
	if _, err := p.store.GetEngine("old1"); err != nil {
		t.Errorf("导入的用户引擎没进来: %v", err)
	}
	if _, err := p.store.DefaultEngine(); err != nil {
		t.Errorf("导入后没有可用默认引擎: %v", err)
	}
}

// TestBuiltin_RegisteredInFactory 复核内置引擎确实注册进工厂且可构造。
func TestBuiltin_RegisteredInFactory(t *testing.T) {
	factory, sch, ok := engine.Lookup(free.Kind)
	if !ok {
		t.Fatal("free 未注册进引擎工厂")
	}
	if !sch.Builtin {
		t.Error("Schema.Builtin 为 false")
	}
	e, err := factory(nil, nil, engine.Options{Name: sch.DefaultName})
	if err != nil {
		t.Fatalf("构造内置引擎失败: %v", err)
	}
	if e.Kind() != free.Kind {
		t.Errorf("Kind() = %q, 期望 %q", e.Kind(), free.Kind)
	}
}
