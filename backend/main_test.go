package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	dbxpluginsdk "github.com/vyrnax/translation/dbxsdk"
	"github.com/vyrnax/translation/engine"
	"github.com/vyrnax/translation/secret"
	"github.com/vyrnax/translation/store"
)

// newTestPlugin 构造一个带独立数据目录的 plugin 实例。
// 每个测试用独立目录，避免测试之间通过 SQLite 文件互相污染。
func newTestPlugin(t *testing.T) *plugin {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &plugin{
		store:      st,
		protector:  secret.Default(pluginID),
		dataDir:    dir,
		tasks:      map[string]*task{},
		semaphores: map[string]chan struct{}{},
	}
}

// call 直接调用 Handle，返回解析后的 result 与 error。
func call(p *plugin, method string, params map[string]any) (map[string]any, *dbxpluginsdk.PluginError) {
	raw, _ := json.Marshal(params)
	res, rpcErr := p.Handle(dbxpluginsdk.RequestContext{}, method, raw, nil)
	if rpcErr != nil {
		return nil, rpcErr
	}
	out, _ := res.(map[string]any)
	return out, nil
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<marshal error: %v>", err)
	}
	return string(b)
}

func mustB64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// numEq 比较 JSON 往返后的数值。
//
// 偏好存库要经 json.Marshal/Unmarshal，Go 的 encoding/json 会把所有数字
// 统一解成 float64。因此 `m["x"] != 900` 这种写法【永远为真】——
// 接口比较时 float64(900) 与 int(900) 是不同类型，不相等。
func numEq(v any, want int) bool {
	switch n := v.(type) {
	case float64:
		return n == float64(want)
	case int:
		return n == want
	case int64:
		return n == int64(want)
	}
	return false
}

func mustBase64Decode(t *testing.T, s string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64 解码失败: %v", err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Handle 分发
// ---------------------------------------------------------------------------

// TestHandle_UnknownMethod 验证未知方法返回 -32601。
//
// 静默无响应比返回错误更危险：前端的 invoke 会一直挂到超时，
// 用户只看到「卡住了」，完全没有线索。
func TestHandle_UnknownMethod(t *testing.T) {
	p := newTestPlugin(t)
	for _, m := range []string{"no/such-method", "translate", "ENGINE/LIST"} {
		_, rpcErr := p.Handle(dbxpluginsdk.RequestContext{}, m, nil, nil)
		if rpcErr == nil {
			t.Errorf("方法 %q 应返回错误", m)
			continue
		}
		if rpcErr.Code != -32601 {
			t.Errorf("方法 %q 的错误码 = %d, 期望 -32601", m, rpcErr.Code)
		}
	}
}

// TestHandle_InvalidParams 验证非法 JSON 参数返回 -32602 而非 panic。
func TestHandle_InvalidParams(t *testing.T) {
	p := newTestPlugin(t)
	_, rpcErr := p.Handle(dbxpluginsdk.RequestContext{}, "engines/get",
		json.RawMessage(`{这不是JSON`), nil)
	if rpcErr == nil {
		t.Fatal("非法 JSON 应返回错误")
	}
	if rpcErr.Code != -32602 {
		t.Errorf("错误码 = %d, 期望 -32602", rpcErr.Code)
	}
}

// TestHandle_AllMethodsDispatch 验证 RPC 全表里的每个方法都能被分发。
//
// 防的是「加了新方法但忘了在 switch 里注册」—— 那种漏注册的症状是
// 前端调用超时，而不是明确的错误，极难排查。
func TestHandle_AllMethodsDispatch(t *testing.T) {
	p := newTestPlugin(t)
	for _, m := range []string{"translate/ping", "translate/schema", "engines/list", "prefs/get"} {
		if _, rpcErr := p.Handle(dbxpluginsdk.RequestContext{}, m, json.RawMessage(`{}`), nil); rpcErr != nil {
			t.Errorf("方法 %s 不应报错，得到 code=%d msg=%s", m, rpcErr.Code, rpcErr.Message)
		}
	}
	// 需要参数的方法：缺参数应返回 -32602 而不是 -32601（证明它确实被分发了）
	for _, m := range []string{
		"engines/get", "engines/delete", "engines/setDefault", "engines/toggle",
		"engines/reorder", "translate/cancel", "history/star", "history/delete",
		"prefs/set", "data/import",
	} {
		_, rpcErr := p.Handle(dbxpluginsdk.RequestContext{}, m, json.RawMessage(`{}`), nil)
		if rpcErr == nil {
			t.Errorf("方法 %s 缺参数应报错", m)
			continue
		}
		if rpcErr.Code == -32601 {
			t.Errorf("方法 %s 未被分发（返回了 -32601 方法不存在）", m)
		}
	}
}

// TestHandle_NoStore 验证存储不可用时给出可操作的错误而非 panic。
//
// 侧车在数据库打不开时【仍然会启动】（否则用户连错误提示都看不到），
// 因此每个需要存储的方法都必须先过 requireStore 这道检查。
func TestHandle_NoStore(t *testing.T) {
	p := &plugin{
		store:      nil,
		protector:  secret.Default(pluginID),
		dataDir:    t.TempDir(),
		tasks:      map[string]*task{},
		semaphores: map[string]chan struct{}{},
	}
	for _, m := range []string{"engines/list", "prefs/get", "history/list", "translate/run", "data/export"} {
		_, rpcErr := p.Handle(dbxpluginsdk.RequestContext{}, m, json.RawMessage(`{}`), nil)
		if rpcErr == nil {
			t.Errorf("方法 %s 在无存储时应报错", m)
			continue
		}
		if rpcErr.Code != -32002 {
			t.Errorf("方法 %s 的错误码 = %d, 期望 -32002", m, rpcErr.Code)
		}
		if !strings.Contains(rpcErr.Message, "数据存储不可用") {
			t.Errorf("方法 %s 的错误消息应可操作: %s", m, rpcErr.Message)
		}
	}
	// ping 不应报错：它的职责就是报告 dbOk=false
	res, rpcErr := call(p, "translate/ping", nil)
	if rpcErr != nil {
		t.Fatalf("ping 不应报错: %v", rpcErr)
	}
	if res["dbOk"] != false {
		t.Errorf("无存储时 dbOk 应为 false，得到 %v", res["dbOk"])
	}
}

// ---------------------------------------------------------------------------
// ping 与 schema
// ---------------------------------------------------------------------------

func TestPing_Fields(t *testing.T) {
	p := newTestPlugin(t)
	res, rpcErr := call(p, "translate/ping", nil)
	if rpcErr != nil {
		t.Fatalf("ping 报错: %v", rpcErr)
	}
	if res["ok"] != true {
		t.Error("ok 应为 true")
	}
	if res["plugin"] != pluginID {
		t.Errorf("plugin = %v, 期望 %v", res["plugin"], pluginID)
	}
	if res["dbOk"] != true {
		t.Error("dbOk 应为 true")
	}
	// 密钥方案必须上报，前端据此决定是否显示「仅混淆保护」警示条
	if s, _ := res["secretScheme"].(string); s == "" {
		t.Error("secretScheme 不应为空")
	}
	if _, ok := res["secretSecure"].(bool); !ok {
		t.Error("secretSecure 必须是布尔值")
	}
	if _, ok := res["inflight"].(int); !ok {
		t.Errorf("inflight 应为 int，得到 %T", res["inflight"])
	}
}

func TestSchema_Shape(t *testing.T) {
	p := newTestPlugin(t)
	res, rpcErr := call(p, "translate/schema", nil)
	if rpcErr != nil {
		t.Fatalf("schema 报错: %v", rpcErr)
	}
	kinds, ok := res["kinds"].([]map[string]any)
	if !ok || len(kinds) == 0 {
		t.Fatalf("kinds 缺失或为空: %T", res["kinds"])
	}
	if _, ok := res["languages"]; !ok {
		t.Error("缺少 languages")
	}
	for _, k := range kinds {
		if k["kind"] == "" || k["label"] == "" {
			t.Errorf("引擎 schema 不完整: %v", k["kind"])
		}
		if _, ok := k["fields"]; !ok {
			t.Errorf("引擎 %v 缺少 fields", k["kind"])
		}
		if _, ok := k["languages"]; !ok {
			t.Errorf("引擎 %v 缺少 languages（前端要靠它禁用不支持的选项）", k["kind"])
		}
	}
}

// ---------------------------------------------------------------------------
// 引擎管理与密钥安全
// ---------------------------------------------------------------------------

// TestEngines_SaveNeverLeaksSecret 是最关键的安全测试：
// 走完整条 save → list → get 链路，确认密钥明文不出现在任何返回值与磁盘文件里。
func TestEngines_SaveNeverLeaksSecret(t *testing.T) {
	p := newTestPlugin(t)
	const fakeKey = "sk-test-SECRETVALUE-987654321"

	res, rpcErr := call(p, "engines/save", map[string]any{
		"name":    "测试引擎",
		"kind":    "openai",
		"config":  map[string]any{"endpoint": "https://api.example.com/v1", "model": "m"},
		"secrets": map[string]any{"apiKey": fakeKey},
		"rev":     -1,
	})
	if rpcErr != nil {
		t.Fatalf("save 报错: %v", rpcErr)
	}
	if res["ok"] != true {
		t.Fatalf("save 未成功: %v", res)
	}
	if strings.Contains(mustJSON(res), fakeKey) {
		t.Errorf("save 返回值泄露密钥明文: %s", mustJSON(res))
	}
	fp, _ := res["fingerprint"].(string)
	if fp == "" {
		t.Error("应返回 fingerprint")
	}
	if strings.Contains(fp, "SECRETVALUE") {
		t.Errorf("fingerprint 泄露密钥中段: %s", fp)
	}

	id, _ := res["id"].(string)
	listRes, _ := call(p, "engines/list", nil)
	if strings.Contains(mustJSON(listRes), fakeKey) {
		t.Error("engines/list 泄露密钥明文")
	}
	getRes, _ := call(p, "engines/get", map[string]any{"id": id})
	if strings.Contains(mustJSON(getRes), fakeKey) {
		t.Error("engines/get 泄露密钥明文")
	}

	// 数据库文件里也不能有明文（含 WAL：库开了 WAL 模式，
	// 进程运行期间新数据都在 -wal 里，只查主库会漏）
	for _, name := range []string{"translation.db", "translation.db-wal", "translation.db-shm"} {
		b, err := os.ReadFile(filepath.Join(p.dataDir, name))
		if err != nil {
			continue
		}
		if strings.Contains(string(b), fakeKey) {
			t.Errorf("%s 中存在密钥明文", name)
		}
	}

	// 密钥确实被加密保存了（能解回原文）
	rec, err := p.store.GetSecret(id)
	if err != nil {
		t.Fatalf("读取密钥记录失败: %v", err)
	}
	if strings.Contains(rec.Cipher, fakeKey) {
		t.Error("密文里含明文密钥")
	}
	plain, err := p.protector.Unprotect(rec.Cipher)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if string(plain) != fakeKey {
		t.Error("解密后与原文不一致")
	}
}

// TestEngines_SecretSemantics 验证「不传=保持、空串=清除、有值=更新」三态。
//
// 这个区分很重要：用户改引擎名称时不应该被迫重新输入 API Key。
func TestEngines_SecretSemantics(t *testing.T) {
	p := newTestPlugin(t)
	base := func(id string, sec map[string]any) map[string]any {
		m := map[string]any{
			"name": "e", "kind": "openai",
			"config": map[string]any{"endpoint": "https://a.com/v1", "model": "m"},
			"rev":    -1,
		}
		if id != "" {
			m["id"] = id
		}
		if sec != nil {
			m["secrets"] = sec
		}
		return m
	}

	res, _ := call(p, "engines/save", base("", map[string]any{"apiKey": "sk-first-1234567890"}))
	id, _ := res["id"].(string)

	// 不传 secrets → 保持不变
	res2, _ := call(p, "engines/save", base(id, nil))
	if res2["secretAction"] != "unchanged" {
		t.Errorf("不传 secrets 时 secretAction = %v, 期望 unchanged", res2["secretAction"])
	}
	if res2["hasSecret"] != true {
		t.Error("密钥应保持")
	}

	// 传新值 → 更新
	res3, _ := call(p, "engines/save", base(id, map[string]any{"apiKey": "sk-second-99999999"}))
	if res3["secretAction"] != "updated" {
		t.Errorf("传新值时 secretAction = %v, 期望 updated", res3["secretAction"])
	}

	// 传空串 → 清除
	res4, _ := call(p, "engines/save", base(id, map[string]any{"apiKey": ""}))
	if res4["secretAction"] != "cleared" {
		t.Errorf("传空串时 secretAction = %v, 期望 cleared", res4["secretAction"])
	}
	if res4["hasSecret"] != false {
		t.Error("密钥应已清除")
	}
	if _, err := p.store.GetSecret(id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("secrets 表应已删除记录，得到 %v", err)
	}
}

// TestEngines_Validation 验证配置校验。
func TestEngines_Validation(t *testing.T) {
	p := newTestPlugin(t)
	if _, e := call(p, "engines/save", map[string]any{
		"name": "x", "kind": "不存在的引擎", "rev": -1,
	}); e == nil || e.Code != -32602 {
		t.Errorf("未知 kind 应返回 -32602，得到 %v", e)
	}
	if _, e := call(p, "engines/save", map[string]any{"name": "x", "rev": -1}); e == nil {
		t.Error("缺 kind 应报错")
	}
	// 端点非法（非 http/https）应在保存时就被拦下，而不是等翻译时才失败
	if _, e := call(p, "engines/save", map[string]any{
		"name": "x", "kind": "openai", "rev": -1,
		"config":  map[string]any{"endpoint": "ftp://a.com", "model": "m"},
		"secrets": map[string]any{"apiKey": "sk-x-1234567890"},
	}); e == nil {
		t.Error("非法 scheme 应在保存时被拒绝")
	}
}

// TestEngines_WarningOnInsecure 验证非加密连接与跳过证书校验会返回警告。
func TestEngines_WarningOnInsecure(t *testing.T) {
	p := newTestPlugin(t)
	res, rpcErr := call(p, "engines/save", map[string]any{
		"name": "x", "kind": "openai", "rev": -1,
		// 本地端点允许 http，但仍应给出提示
		"config": map[string]any{"endpoint": "http://192.168.1.10:8080/v1", "model": "m", "skipTlsVerify": true},
	})
	if rpcErr != nil {
		t.Fatalf("保存失败: %v", rpcErr)
	}
	w, _ := res["warning"].(string)
	if !strings.Contains(w, "TLS") {
		t.Errorf("开启 skipTlsVerify 时应返回警告，得到 %q", w)
	}
}

// TestEngines_Conflict 验证 rev 冲突时返回特殊结构且不覆盖数据。
func TestEngines_Conflict(t *testing.T) {
	p := newTestPlugin(t)
	res, _ := call(p, "engines/save", map[string]any{
		"name": "原名", "kind": "openai",
		"config": map[string]any{"endpoint": "https://a.com/v1", "model": "m"},
		"rev":    -1,
	})
	id, _ := res["id"].(string)

	res2, rpcErr := call(p, "engines/save", map[string]any{
		"id": id, "name": "被覆盖", "kind": "openai",
		"config": map[string]any{"endpoint": "https://a.com/v1", "model": "m"},
		"rev":    0, // 过期
	})
	// 冲突被设计成【业务结果】而非协议错误：
	// SDK 的 Emitter.respond() 在 pluginError != nil 时会丢弃 result，
	// 若同时返回两者，前端就拿不到 newRev、无法自动刷新重试。
	if rpcErr != nil {
		t.Errorf("冲突应作为业务结果返回（rpcErr 应为 nil），得到 %v", rpcErr)
	}
	if res2 == nil || res2["conflict"] != true {
		t.Fatalf("冲突时应返回 conflict 结构供前端重试，得到 %v", res2)
	}
	if res2["ok"] != false {
		t.Error("冲突时 ok 应为 false")
	}
	if c, _ := res2["code"].(int); c != -32020 {
		t.Errorf("冲突的 code = %v, 期望 -32020", res2["code"])
	}
	if _, ok := res2["newRev"].(int); !ok {
		t.Errorf("冲突响应应带 int 类型的 newRev，得到 %T", res2["newRev"])
	}
	got, _ := call(p, "engines/get", map[string]any{"id": id})
	eng, _ := got["engine"].(map[string]any)
	if eng["name"] != "原名" {
		t.Errorf("冲突时数据被覆盖了: %v", eng["name"])
	}
}

// TestEngines_DisabledRejectsTranslate 验证禁用的引擎不能用来翻译。
func TestEngines_DisabledRejectsTranslate(t *testing.T) {
	p := newTestPlugin(t)
	res, _ := call(p, "engines/save", map[string]any{
		"name": "e", "kind": "openai",
		"config":  map[string]any{"endpoint": "https://a.com/v1", "model": "m"},
		"secrets": map[string]any{"apiKey": "sk-x-1234567890"},
		"rev":     -1,
	})
	id, _ := res["id"].(string)
	call(p, "engines/toggle", map[string]any{"id": id, "enabled": false, "rev": -1})

	_, rpcErr := call(p, "translate/run", map[string]any{
		"engineId": id, "text": "hi", "sourceLang": "en", "targetLang": "zh",
	})
	if rpcErr == nil {
		t.Fatal("禁用的引擎不应能翻译")
	}
	if !strings.Contains(rpcErr.Message, "禁用") {
		t.Errorf("错误消息应说明引擎已禁用: %s", rpcErr.Message)
	}
}

// TestEngines_NoEngineMessage 验证没有引擎时给出可操作的引导。
func TestEngines_NoEngineMessage(t *testing.T) {
	p := newTestPlugin(t)
	_, rpcErr := call(p, "translate/run", map[string]any{
		"text": "hi", "sourceLang": "en", "targetLang": "zh",
	})
	if rpcErr == nil {
		t.Fatal("无引擎时应报错")
	}
	if rpcErr.Code != -32037 {
		t.Errorf("错误码 = %d, 期望 -32037", rpcErr.Code)
	}
	if !strings.Contains(rpcErr.Message, "引擎管理") {
		t.Errorf("错误消息应引导用户去添加引擎: %s", rpcErr.Message)
	}
}

// TestEngines_UnsupportedLangRejected 验证不支持的语言被提前拦截。
//
// 提前拦截比让上游报「不支持的语言」更友好 —— 上游的错误消息
// 往往是英文的错误码，用户看不懂。
func TestEngines_UnsupportedLangRejected(t *testing.T) {
	p := newTestPlugin(t)
	res, _ := call(p, "engines/save", map[string]any{
		"name": "e", "kind": "openai",
		"config":  map[string]any{"endpoint": "https://a.com/v1", "model": "m"},
		"secrets": map[string]any{"apiKey": "sk-x-1234567890"},
		"rev":     -1,
	})
	id, _ := res["id"].(string)
	_, rpcErr := call(p, "translate/run", map[string]any{
		"engineId": id, "text": "hi", "sourceLang": "en", "targetLang": "auto",
	})
	if rpcErr == nil {
		t.Fatal("目标语言为 auto 应被拒绝")
	}
}

// ---------------------------------------------------------------------------
// 参数解析辅助
// ---------------------------------------------------------------------------

func TestIntArg_Clamping(t *testing.T) {
	cases := []struct {
		in   any
		want int
	}{
		{float64(500), 500},
		{float64(10), 100},      // 低于下限
		{float64(999999), 5000}, // 高于上限
		{int(300), 300},
		{"400", 400}, // 字符串数字也要接
		{"abc", 200}, // 非法字符串回退默认
		{true, 200},  // 布尔回退默认
	}
	for _, c := range cases {
		if got := intArg(map[string]any{"n": c.in}, "n", 200, 100, 5000); got != c.want {
			t.Errorf("intArg(%v) = %d, 期望 %d", c.in, got, c.want)
		}
	}
	if got := intArg(map[string]any{"n": nil}, "n", 200, 100, 5000); got != 200 {
		t.Errorf("nil 应回退默认，得到 %d", got)
	}
	if got := intArg(map[string]any{}, "n", 200, 100, 5000); got != 200 {
		t.Errorf("缺失键应回退默认，得到 %d", got)
	}
	// NaN 必须拦掉，否则夹取比较的结果不可预期。
	// 注意：NaN 不能写成 Go 常量表达式 0.0/0.0（编译期常量折叠会直接报
	// "division by zero"），必须用 math.NaN() 在运行期构造。
	if got := intArg(map[string]any{"n": math.NaN()}, "n", 200, 100, 5000); got != 200 {
		t.Errorf("NaN 应回退默认，得到 %d", got)
	}
	if got := intArg(map[string]any{"n": math.Inf(1)}, "n", 200, 100, 5000); got != 200 {
		t.Errorf("+Inf 应回退默认，得到 %d", got)
	}
}

func TestRevArg(t *testing.T) {
	if got := revArg(map[string]any{}); got != -1 {
		t.Errorf("缺失 rev 应返回 -1（跳过校验），得到 %d", got)
	}
	if got := revArg(map[string]any{"rev": nil}); got != -1 {
		t.Errorf("null rev 应返回 -1，得到 %d", got)
	}
	if got := revArg(map[string]any{"rev": float64(5)}); got != 5 {
		t.Errorf("rev = %d, 期望 5", got)
	}
	if got := revArg(map[string]any{"rev": "非法"}); got != -1 {
		t.Errorf("非法 rev 应返回 -1，得到 %d", got)
	}
}

func TestExtractSecrets(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want string
	}{
		{"apiKey", map[string]any{"apiKey": "sk-1"}, "sk-1"},
		{"secretKey", map[string]any{"secretKey": "sk-2"}, "sk-2"},
		{"appSecret", map[string]any{"appSecret": "sk-3"}, "sk-3"},
		{"accessKeySecret", map[string]any{"accessKeySecret": "sk-4"}, "sk-4"},
		{"首尾空白被去除", map[string]any{"apiKey": "  sk-5\n"}, "sk-5"},
		{"优先级：apiKey 胜过 token", map[string]any{"apiKey": "a", "token": "b"}, "a"},
		{"空值跳过", map[string]any{"apiKey": "", "token": "t"}, "t"},
		{"全空", map[string]any{"apiKey": ""}, ""},
		{"nil", nil, ""},
		{"非字符串值跳过", map[string]any{"apiKey": 123, "key": "k"}, "k"},
	}
	for _, c := range cases {
		if got := extractSecrets(c.in); got != c.want {
			t.Errorf("%s: extractSecrets = %q, 期望 %q", c.name, got, c.want)
		}
	}
}

// TestSecretsFor_AllEnginesCanReadSecret 验证【每个】引擎都能拿到自己的密钥。
//
// 这条测试是针对一个真实线上 bug 补的：buildEngine 曾经硬编码
// `Secrets{"apiKey":…, "secretKey":…, "appSecret":…}` 三个键，
// 而阿里云适配器读的是 accessKeySecret —— 结果阿里云引擎永远拿不到密钥，
// 用户明明填了 AccessKey Secret，点「测试连接」却报「不能为空」。
//
// 这类 bug 极隐蔽：只影响一个引擎，其余七个都正常，
// 只要测试没逐个引擎覆盖就发现不了。因此这里遍历【全部已注册引擎】，
// 对每个引擎都走一遍「构造 → 取密钥」，确保 schema 声明的 Secret 字段名
// 与适配器实际读取的键名对得上。
func TestSecretsFor_AllEnginesCanReadSecret(t *testing.T) {
	const fakeSecret = "sk-regression-test-secret-value"

	kinds := engine.Kinds()
	if len(kinds) < 8 {
		t.Fatalf("已注册引擎数 = %d, 期望至少 8（本测试的意义在于全覆盖）", len(kinds))
	}

	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			_, sch, ok := engine.Lookup(kind)
			if !ok {
				t.Fatalf("Lookup(%q) 失败", kind)
			}

			// 找出 schema 里声明的 Secret 字段
			var secretKeys []string
			for _, f := range sch.Fields {
				if f.Secret {
					secretKeys = append(secretKeys, f.Key)
				}
			}

			// 不需要密钥的引擎（localcli）：secretsFor 应返回空表，且不报错
			if !sch.NeedsSecret {
				got := secretsFor(kind, fakeSecret)
				if len(secretKeys) != 0 {
					t.Errorf("NeedsSecret=false 但 schema 里有 Secret 字段: %v", secretKeys)
				}
				_ = got
				return
			}
			if len(secretKeys) == 0 {
				t.Fatal("NeedsSecret=true 但 schema 里没有 Secret 字段")
			}

			// 核心断言：secretsFor 产出的表必须覆盖 schema 声明的每个 Secret 键
			got := secretsFor(kind, fakeSecret)
			for _, k := range secretKeys {
				if got[k] != fakeSecret {
					t.Errorf("引擎 %q 的密钥键 %q 未被填充（适配器会读到空值，"+
						"表现为「用户填了密钥却报不能为空」）。secretsFor 返回的键: %v",
						kind, k, keysOf(got))
				}
			}
		})
	}
}

// TestSecretsFor_EmptyPlain 验证空密钥返回空表而非塞入空值。
//
// 塞入空值会让适配器的「密钥不能为空」校验失效判断变得混乱。
func TestSecretsFor_EmptyPlain(t *testing.T) {
	got := secretsFor("openai", "")
	if len(got) != 0 {
		t.Errorf("空密钥应返回空表，得到 %v", keysOf(got))
	}
	// 未知 kind 也不应 panic
	if got := secretsFor("不存在的引擎", "x"); len(got) == 0 {
		t.Error("未知 kind 应走兜底分支填充常见键名")
	}
}

func keysOf(m engine.Secrets) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestNewID_Unique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		id := newID("eng")
		if seen[id] {
			t.Fatalf("第 %d 次生成了重复 ID %q", i, id)
		}
		seen[id] = true
		if !strings.HasPrefix(id, "eng-") {
			t.Errorf("ID 前缀错误: %q", id)
		}
	}
}

// TestDefaultEngineName 验证默认引擎名来自 schema（单一事实来源）。
//
// 【刻意不断言具体名称字符串】：defaultEngineName 现在从 schema.DefaultName 读取，
// 断言 "大模型翻译" 这类字面值只会把测试和文案绑死 —— 改个显示名就要改测试，
// 而测试真正该保证的是「后端兜底名与前端自动补全名是同一个值」。
//
// 这条不变量很重要：前端选类型时自动填入 schema.defaultName，
// 用户留空保存时后端用 defaultEngineName(kind) 兜底。
// 两者若不一致，用户会看到「填的时候是 A，保存后变成 B」。
func TestDefaultEngineName(t *testing.T) {
	kinds := engine.Kinds()
	if len(kinds) == 0 {
		t.Fatal("没有注册任何引擎")
	}
	for _, kind := range kinds {
		_, sch, ok := engine.Lookup(kind)
		if !ok {
			t.Errorf("Lookup(%q) 失败", kind)
			continue
		}
		got := defaultEngineName(kind)
		if got == "" {
			t.Errorf("引擎 %q 的默认名为空（列表里会出现空白行）", kind)
			continue
		}
		// 核心不变量：后端兜底名必须等于 schema 里的默认名
		if sch.DefaultName != "" && got != sch.DefaultName {
			t.Errorf("引擎 %q: defaultEngineName=%q 与 schema.DefaultName=%q 不一致"+
				"（前端自动补全与后端兜底会给出不同名称）", kind, got, sch.DefaultName)
		}
		// 默认名不应是原始 kind（那说明 schema 没配 DefaultName，用户会看到 "aliyun"）
		if got == kind {
			t.Errorf("引擎 %q 的默认名等于 kind 本身，说明 schema 缺少 DefaultName", kind)
		}
	}

	// 未知类型回退为 kind 本身（不 panic、不返回空）
	if got := defaultEngineName("未知类型"); got != "未知类型" {
		t.Errorf("未知类型应回退为 kind 本身，得到 %q", got)
	}
	if got := defaultEngineName(""); got != "" {
		t.Errorf("空 kind 应返回空串，得到 %q", got)
	}
}

// TestSchema_AllKindsHaveDefaultName 验证每个已注册引擎都配了 defaultName。
//
// 这是「选类型自动补全名称」功能的前提：缺了它，前端只能填入 kind 原文
// （例如 "aliyun"），用户看到一串英文小写标识会以为是 bug。
func TestSchema_AllKindsHaveDefaultName(t *testing.T) {
	for _, sch := range engine.Schemas() {
		if sch.DefaultName == "" {
			t.Errorf("引擎 %q 的 schema 缺少 DefaultName（前端无法自动补全名称）", sch.Kind)
		}
		if sch.DefaultNameEn == "" {
			t.Errorf("引擎 %q 的 schema 缺少 DefaultNameEn（英文界面下无法自动补全）", sch.Kind)
		}
		if sch.Label == "" {
			t.Errorf("引擎 %q 的 schema 缺少 Label", sch.Kind)
		}
	}
}

// ---------------------------------------------------------------------------
// 任务表：并发与泄漏防护
// ---------------------------------------------------------------------------

// TestTaskTable_NoLeak 验证任务被正确清理。
//
// 泄漏的后果很严重：tasks 表无限增长 → 触发 maxInflightTasks →
// 之后所有翻译请求都被拒绝，插件变成「永久不可用」，重启侧车才能恢复。
func TestTaskTable_NoLeak(t *testing.T) {
	p := newTestPlugin(t)
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("t%d", i)
		_, cancel := context.WithCancel(context.Background())
		p.mu.Lock()
		p.tasks[id] = &task{id: id, cancel: cancel, engineID: "e", started: time.Now()}
		p.mu.Unlock()
	}
	if p.taskCount() != 10 {
		t.Fatalf("任务数 = %d, 期望 10", p.taskCount())
	}
	for i := 0; i < 10; i++ {
		p.removeTask(fmt.Sprintf("t%d", i))
	}
	if p.taskCount() != 0 {
		t.Errorf("清理后任务数 = %d, 期望 0", p.taskCount())
	}
	// 重复移除不应 panic（取消与完成可能同时发生）
	p.removeTask("不存在")
	p.removeTask("t0")
	if p.taskCount() != 0 {
		t.Error("重复移除后任务数应仍为 0")
	}
}

// TestTaskID_Unique 验证 taskId 唯一（前端靠它做竞态过滤）。
func TestTaskID_Unique(t *testing.T) {
	p := newTestPlugin(t)
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		id := p.newTaskID()
		if seen[id] {
			t.Fatalf("第 %d 次生成了重复 taskId %q", i, id)
		}
		seen[id] = true
	}
}

// TestTaskTable_ConcurrentAccess 验证并发增删不死锁。
//
// SDK 对每个请求开一个 goroutine，任务表的读写天然并发。
// 死锁的症状是侧车完全无响应，用户只能重启 —— 必须在测试里兜住。
func TestTaskTable_ConcurrentAccess(t *testing.T) {
	p := newTestPlugin(t)
	var wg sync.WaitGroup
	const n = 50
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := p.newTaskID()
			_, cancel := context.WithCancel(context.Background())
			p.mu.Lock()
			p.tasks[id] = &task{id: id, cancel: cancel, started: time.Now()}
			p.mu.Unlock()
			_ = p.taskCount()
			p.removeTask(id)
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("并发访问任务表死锁（20 秒未完成）")
	}
	if p.taskCount() != 0 {
		t.Errorf("并发结束后任务数 = %d, 期望 0", p.taskCount())
	}
}

// TestInflightLimit 验证在飞任务上限生效。
//
// 没有上限的话，快速打字触发的任务会无限堆积 goroutine 与上游连接。
func TestInflightLimit(t *testing.T) {
	p := newTestPlugin(t)
	res, _ := call(p, "engines/save", map[string]any{
		"name": "e", "kind": "openai",
		"config":  map[string]any{"endpoint": "https://a.com/v1", "model": "m"},
		"secrets": map[string]any{"apiKey": "sk-x-1234567890"},
		"rev":     -1,
	})
	id, _ := res["id"].(string)

	// 塞满在飞任务表
	for i := 0; i < maxInflightTasks; i++ {
		tid := fmt.Sprintf("busy%d", i)
		_, cancel := context.WithCancel(context.Background())
		p.mu.Lock()
		p.tasks[tid] = &task{id: tid, cancel: cancel, started: time.Now()}
		p.mu.Unlock()
	}

	out, rpcErr := call(p, "translate/start", map[string]any{
		"engineId": id, "text": "hi", "sourceLang": "en", "targetLang": "zh",
	})
	// 应返回【业务失败】而非 RPC 错误：前端要能展示可操作的提示
	if rpcErr != nil {
		t.Fatalf("应返回业务失败而非 RPC 错误: %v", rpcErr)
	}
	if out["ok"] != false {
		t.Errorf("超出在飞上限时应拒绝，得到 %v", out)
	}
	if c, _ := out["code"].(int); c != -32032 {
		t.Errorf("错误码 = %v, 期望 -32032", out["code"])
	}
}

// ---------------------------------------------------------------------------
// 信号量
// ---------------------------------------------------------------------------

func TestSemaphore(t *testing.T) {
	p := newTestPlugin(t)
	s1 := p.semaphore("openai", 2)
	if cap(s1) != 2 {
		t.Errorf("容量 = %d, 期望 2", cap(s1))
	}
	if p.semaphore("openai", 2) != s1 {
		t.Error("同 kind 应复用同一信号量")
	}
	if p.semaphore("deepl", 1) == s1 {
		t.Error("不同 kind 应使用独立信号量（各服务商配额独立）")
	}
	if cap(p.semaphore("x1", 0)) != 1 {
		t.Error("容量 0 应夹到 1")
	}
	if cap(p.semaphore("x2", 99)) != 4 {
		t.Error("容量 99 应夹到 4")
	}

	sem := make(chan struct{}, 1)
	ctx := context.Background()
	if err := acquire(ctx, sem); err != nil {
		t.Fatalf("acquire 失败: %v", err)
	}
	// 已满 + ctx 已取消 → 应立即返回而不是永久阻塞
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := acquire(cctx, sem); err == nil {
		t.Error("信号量已满且 ctx 已取消时 acquire 应报错")
	}
	release(sem)
	if err := acquire(ctx, sem); err != nil {
		t.Errorf("release 后应能再次 acquire: %v", err)
	}
	// 重复 release 不应 panic
	release(sem)
	release(sem)
}

// ---------------------------------------------------------------------------
// 端到端翻译（经 Handle，用 httptest 假端点）
// ---------------------------------------------------------------------------

func newFakeUpstream(t *testing.T, reply string) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, part := range []string{"你", "好"} {
				b, _ := json.Marshal(map[string]any{
					"choices": []map[string]any{{"delta": map[string]any{"content": part}}},
				})
				_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	return srv, srv.URL + "/v1/chat/completions"
}

func setupEngine(t *testing.T, p *plugin, endpoint string) string {
	t.Helper()
	res, rpcErr := call(p, "engines/save", map[string]any{
		"name": "e2e", "kind": "openai",
		"config":  map[string]any{"endpoint": endpoint, "model": "m"},
		"secrets": map[string]any{"apiKey": "sk-e2e-1234567890"},
		"rev":     -1,
	})
	if rpcErr != nil {
		t.Fatalf("建引擎失败: %v", rpcErr)
	}
	id, _ := res["id"].(string)
	return id
}

func TestTranslateRun_EndToEnd(t *testing.T) {
	p := newTestPlugin(t)
	srv, endpoint := newFakeUpstream(t, `{"choices":[{"message":{"content":"你好世界"},"finish_reason":"stop"}]}`)
	defer srv.Close()
	id := setupEngine(t, p, endpoint)

	out, rpcErr := call(p, "translate/run", map[string]any{
		"engineId": id, "text": "hello world", "sourceLang": "en", "targetLang": "zh",
	})
	if rpcErr != nil {
		t.Fatalf("翻译报错: %v", rpcErr)
	}
	if out["ok"] != true {
		t.Fatalf("翻译未成功: %v", out)
	}
	if out["target"] != "你好世界" {
		t.Errorf("译文 = %v", out["target"])
	}
	// 应写入历史
	hist, _ := call(p, "history/list", map[string]any{"limit": 10})
	items, _ := hist["items"].([]store.HistoryItem)
	if len(items) != 1 {
		t.Errorf("历史应有 1 条，得到 %d", len(items))
	}
	// 历史里不含密钥
	if strings.Contains(mustJSON(hist), "sk-e2e-1234567890") {
		t.Error("历史记录泄露了密钥")
	}
}

// TestTranslateRun_PreservesDetectedLangAndModel 验证分段翻译不丢失适配器返回的元信息。
//
// 这是线上验证发现的 bug：runChunked 重建 Result 时只填了
// Target/ElapsedMs/Chunks/Engine，把适配器返回的 DetectedLang 与 Model 丢掉了。
// 症状是 translate/run 响应里 detectedLang 与 model 永远是空串，
// 「自动检测到的源语言」完全展示不出来。
//
// 单元测试当时没抓到，是因为它们直接测适配器（Translate 返回的 Result 是对的），
// 而没有测「经过 runChunked 之后」的 Result —— 分层测试的盲区。
func TestTranslateRun_PreservesDetectedLangAndModel(t *testing.T) {
	p := newTestPlugin(t)
	// 假端点返回 detected_source_language，模拟 DeepL/Google 这类会回报源语言的引擎
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"你好世界"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	id := setupEngine(t, p, srv.URL+"/v1/chat/completions")

	out, rpcErr := call(p, "translate/run", map[string]any{
		"engineId": id, "text": "hello world", "sourceLang": "en", "targetLang": "zh",
	})
	if rpcErr != nil {
		t.Fatalf("翻译报错: %v", rpcErr)
	}
	// model 必须被保留（openai 适配器会填模型名）
	if m, _ := out["model"].(string); m == "" {
		t.Errorf("model 为空 —— runChunked 丢失了适配器返回的 Model 字段（响应: %v）", out)
	}
	// engineKind 必须有值
	if k, _ := out["engineKind"].(string); k == "" {
		t.Error("engineKind 为空")
	}
}

func TestTranslateRun_RejectsInvalidInput(t *testing.T) {
	p := newTestPlugin(t)
	srv, endpoint := newFakeUpstream(t, `{"choices":[{"message":{"content":"x"}}]}`)
	defer srv.Close()
	id := setupEngine(t, p, endpoint)

	cases := []struct {
		name   string
		params map[string]any
	}{
		{"空文本", map[string]any{"engineId": id, "text": "", "targetLang": "zh"}},
		{"纯空白", map[string]any{"engineId": id, "text": "   ", "targetLang": "zh"}},
		{"缺目标语言", map[string]any{"engineId": id, "text": "hi"}},
		{"目标语言 auto", map[string]any{"engineId": id, "text": "hi", "targetLang": "auto"}},
		{"非法源语言", map[string]any{"engineId": id, "text": "hi", "sourceLang": "克林贡", "targetLang": "zh"}},
		{"不存在的引擎", map[string]any{"engineId": "不存在", "text": "hi", "targetLang": "zh"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, rpcErr := call(p, "translate/run", c.params); rpcErr == nil {
				t.Fatal("应报错")
			}
		})
	}
}

// TestTranslateRun_OversizedText 验证超大文本被前置拦截。
//
// 桥接层单次 JSON 上限 2 MiB、SDK 单条消息上限 8 MB。
// 不拦截的话请求能进来但响应发不出去，表现为「翻译卡住」而非明确报错。
func TestTranslateRun_OversizedText(t *testing.T) {
	p := newTestPlugin(t)
	id := setupEngine(t, p, "https://a.com/v1/chat/completions")

	big := strings.Repeat("字", maxRequestTextBytes)
	_, rpcErr := call(p, "translate/run", map[string]any{
		"engineId": id, "text": big, "targetLang": "zh",
	})
	if rpcErr == nil {
		t.Fatal("超大文本应被拒绝")
	}
	if rpcErr.Code != -32602 {
		t.Errorf("错误码 = %d, 期望 -32602", rpcErr.Code)
	}
	if !strings.Contains(rpcErr.Message, "分段") {
		t.Errorf("错误消息应建议分段: %s", rpcErr.Message)
	}
}

// TestTranslateStart_NeedsEmitter 验证没有 emitter 时明确报错而非静默失败。
//
// 静默返回 taskId 会让前端永远等不到 chunk，表现为「翻译中」转圈不停。
func TestTranslateStart_NeedsEmitter(t *testing.T) {
	p := newTestPlugin(t)
	id := setupEngine(t, p, "https://a.com/v1/chat/completions")

	raw, _ := json.Marshal(map[string]any{
		"engineId": id, "text": "hi", "sourceLang": "en", "targetLang": "zh",
	})
	_, rpcErr := p.Handle(dbxpluginsdk.RequestContext{}, "translate/start", raw, nil)
	if rpcErr == nil {
		t.Fatal("无 emitter 时应报错")
	}
	if !strings.Contains(rpcErr.Message, "事件推送") {
		t.Errorf("错误消息应说明原因: %s", rpcErr.Message)
	}
}

// TestTranslateCancel_Idempotent 验证取消是幂等的。
//
// 前端的「输入即译」会在每次新输入前取消上一个任务，
// 而上一个任务可能已自然完成 —— 这不是错误，不该弹提示。
func TestTranslateCancel_Idempotent(t *testing.T) {
	p := newTestPlugin(t)
	for i := 0; i < 3; i++ {
		out, rpcErr := call(p, "translate/cancel", map[string]any{"taskId": "不存在的任务"})
		if rpcErr != nil {
			t.Fatalf("第 %d 次取消报错: %v", i, rpcErr)
		}
		if out["ok"] != true {
			t.Errorf("第 %d 次取消应返回 ok=true", i)
		}
		if out["alreadyDone"] != true {
			t.Errorf("第 %d 次取消应标记 alreadyDone", i)
		}
	}
	if _, e := call(p, "translate/cancel", map[string]any{}); e == nil {
		t.Error("缺 taskId 应报错")
	}
}

// ---------------------------------------------------------------------------
// 历史与偏好
// ---------------------------------------------------------------------------

func TestHistory_Operations(t *testing.T) {
	p := newTestPlugin(t)
	// 直接写入历史（绕过翻译流程，聚焦历史 RPC 本身）
	for i := 0; i < 5; i++ {
		if err := p.store.AddHistory(store.HistoryItem{
			ID: fmt.Sprintf("h%d", i), Source: fmt.Sprintf("源文%d", i),
			Target: fmt.Sprintf("译文%d", i), Starred: i == 0,
		}, 0); err != nil {
			t.Fatalf("写入历史失败: %v", err)
		}
	}

	out, _ := call(p, "history/list", map[string]any{"limit": 10})
	if total, _ := out["total"].(int); total != 5 {
		t.Errorf("total = %v, 期望 5", out["total"])
	}
	// 搜索
	out2, _ := call(p, "history/list", map[string]any{"q": "源文1", "limit": 10})
	if total, _ := out2["total"].(int); total != 1 {
		t.Errorf("搜索 total = %v, 期望 1", out2["total"])
	}
	// 收藏筛选
	yes := true
	out3, _ := call(p, "history/list", map[string]any{"starred": yes, "limit": 10})
	if total, _ := out3["total"].(int); total != 1 {
		t.Errorf("收藏筛选 total = %v, 期望 1", out3["total"])
	}
	// 收藏切换
	if r, e := call(p, "history/star", map[string]any{"id": "h1", "starred": true}); e != nil || r["ok"] != true {
		t.Errorf("收藏失败: %v %v", r, e)
	}
	// 批量删除
	if r, e := call(p, "history/delete", map[string]any{"ids": []any{"h2", "h3"}}); e != nil {
		t.Errorf("删除失败: %v", e)
	} else if r["deleted"] != 2 {
		t.Errorf("deleted = %v, 期望 2", r["deleted"])
	}
	// 清空（保留收藏）
	if r, e := call(p, "history/clear", map[string]any{"keepStarred": true}); e != nil {
		t.Errorf("清空失败: %v", e)
	} else if r["deleted"] != 1 {
		t.Errorf("应只删 1 条未收藏，得到 %v", r["deleted"])
	}
	// 缺参数
	if _, e := call(p, "history/star", map[string]any{}); e == nil {
		t.Error("history/star 缺 id 应报错")
	}
	if _, e := call(p, "history/delete", map[string]any{}); e == nil {
		t.Error("history/delete 缺 ids 应报错")
	}
}

func TestPrefs_RoundTrip(t *testing.T) {
	p := newTestPlugin(t)
	out, _ := call(p, "prefs/get", nil)
	prefs, _ := out["prefs"].(map[string]any)
	// 默认值应齐全（PLAN.md 第十章规定的既定参数）
	if prefs["instantDebounceMs"] != 600 {
		t.Errorf("instantDebounceMs 默认 = %v, 期望 600", prefs["instantDebounceMs"])
	}
	if prefs["historyLimit"] != 500 {
		t.Errorf("historyLimit 默认 = %v, 期望 500", prefs["historyLimit"])
	}
	if prefs["chunkDelayMs"] != 1100 {
		t.Errorf("chunkDelayMs 默认 = %v, 期望 1100", prefs["chunkDelayMs"])
	}

	res, rpcErr := call(p, "prefs/set", map[string]any{
		"prefs": map[string]any{"instantDebounceMs": 900, "lastView": "history"},
		"rev":   -1,
	})
	if rpcErr != nil {
		t.Fatalf("prefs/set 报错: %v", rpcErr)
	}
	back, _ := res["prefs"].(map[string]any)
	// 偏好经 JSON 往返后数字统一变 float64，直接 != 900 恒为真
	if !numEq(back["instantDebounceMs"], 900) {
		t.Errorf("往返不一致: %v (%T)", back["instantDebounceMs"], back["instantDebounceMs"])
	}
	// 缺 prefs 参数
	if _, e := call(p, "prefs/set", map[string]any{}); e == nil {
		t.Error("prefs/set 缺 prefs 应报错")
	}
}

// ---------------------------------------------------------------------------
// 导出导入
// ---------------------------------------------------------------------------

func TestDataExport_NoSecretsByDefault(t *testing.T) {
	p := newTestPlugin(t)
	const fakeKey = "sk-export-SECRET-1234567890"
	res, _ := call(p, "engines/save", map[string]any{
		"name": "e", "kind": "openai",
		"config":  map[string]any{"endpoint": "https://a.com/v1", "model": "m"},
		"secrets": map[string]any{"apiKey": fakeKey},
		"rev":     -1,
	})
	_ = res

	out, rpcErr := call(p, "data/export", map[string]any{})
	if rpcErr != nil {
		t.Fatalf("导出失败: %v", rpcErr)
	}
	decoded := mustBase64Decode(t, out["dataBase64"].(string))
	if strings.Contains(decoded, fakeKey) {
		t.Error("默认导出泄露了密钥明文")
	}
	if out["hasSecrets"] != false {
		t.Error("默认导出的 hasSecrets 应为 false")
	}
	var payload exportPayload
	if err := json.Unmarshal([]byte(decoded), &payload); err != nil {
		t.Fatalf("导出内容不是合法 JSON: %v", err)
	}
	if payload.Format != "dbx-translation-export" {
		t.Errorf("format = %q", payload.Format)
	}
	if len(payload.Engines) != 1 {
		t.Errorf("应导出 1 个引擎，得到 %d", len(payload.Engines))
	}
}

func TestDataExport_WithSecretsWarns(t *testing.T) {
	p := newTestPlugin(t)
	const fakeKey = "sk-export-SECRET-1234567890"
	call(p, "engines/save", map[string]any{
		"name": "e", "kind": "openai",
		"config":  map[string]any{"endpoint": "https://a.com/v1", "model": "m"},
		"secrets": map[string]any{"apiKey": fakeKey},
		"rev":     -1,
	})
	out, rpcErr := call(p, "data/export", map[string]any{"includeSecrets": true})
	if rpcErr != nil {
		t.Fatalf("导出失败: %v", rpcErr)
	}
	decoded := mustBase64Decode(t, out["dataBase64"].(string))
	if !strings.Contains(decoded, fakeKey) {
		t.Error("显式要求时应包含密钥（跨机器迁移场景）")
	}
	if !strings.Contains(decoded, "警告") {
		t.Error("含密钥的导出必须带明文警告")
	}
	if out["hasSecrets"] != true {
		t.Error("hasSecrets 应为 true")
	}
}

func TestDataImport_RoundTrip(t *testing.T) {
	src := newTestPlugin(t)
	const fakeKey = "sk-import-SECRET-1234567890"
	call(src, "engines/save", map[string]any{
		"name": "源引擎", "kind": "openai",
		"config":  map[string]any{"endpoint": "https://a.com/v1", "model": "m"},
		"secrets": map[string]any{"apiKey": fakeKey},
		"rev":     -1,
	})
	out, _ := call(src, "data/export", map[string]any{"includeSecrets": true})
	b64 := out["dataBase64"].(string)

	// 导入到全新实例（模拟换机器）
	dst := newTestPlugin(t)
	r, rpcErr := call(dst, "data/import", map[string]any{"dataBase64": b64, "mode": "merge"})
	if rpcErr != nil {
		t.Fatalf("导入失败: %v", rpcErr)
	}
	if r["ok"] != true {
		t.Fatalf("导入未成功: %v", r)
	}
	counts, _ := r["counts"].(map[string]int)
	if counts["engines"] != 1 {
		t.Errorf("导入引擎数 = %v, 期望 1", counts["engines"])
	}
	if counts["secrets"] != 1 {
		t.Errorf("导入密钥数 = %v, 期望 1（应用目标机器的保护器重新加密）", counts["secrets"])
	}

	// 导入后密钥能用目标机器的保护器解开
	list, _ := call(dst, "engines/list", nil)
	engines, _ := list["engines"].([]map[string]any)
	if len(engines) != 1 {
		t.Fatalf("引擎数 = %d", len(engines))
	}
	id, _ := engines[0]["id"].(string)
	rec, err := dst.store.GetSecret(id)
	if err != nil {
		t.Fatalf("读取密钥失败: %v", err)
	}
	plain, err := dst.protector.Unprotect(rec.Cipher)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if string(plain) != fakeKey {
		t.Error("导入后密钥内容不一致")
	}
	if strings.Contains(mustJSON(list), fakeKey) {
		t.Error("导入后的 list 泄露了密钥")
	}
}

func TestDataImport_RejectsGarbage(t *testing.T) {
	p := newTestPlugin(t)
	cases := []struct {
		name string
		in   map[string]any
	}{
		{"缺 dataBase64", map[string]any{}},
		{"非法 base64", map[string]any{"dataBase64": "!!!不是base64!!!"}},
		{"非法 JSON", map[string]any{"dataBase64": mustB64("{不是json")}},
		{"格式不匹配", map[string]any{"dataBase64": mustB64(`{"format":"别的东西","version":1}`)}},
		{"版本不支持", map[string]any{"dataBase64": mustB64(`{"format":"dbx-translation-export","version":99}`)}},
		{"非法 mode", map[string]any{
			"dataBase64": mustB64(`{"format":"dbx-translation-export","version":1}`), "mode": "毁灭"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, rpcErr := call(p, "data/import", c.in); rpcErr == nil {
				t.Fatal("应报错")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// resolveMetadata 与数据目录
// ---------------------------------------------------------------------------

func TestResolveMetadata(t *testing.T) {
	m := resolveMetadata()
	if m.ID != pluginID {
		t.Errorf("ID = %q, 期望 %q", m.ID, pluginID)
	}
	if m.Version == "" {
		t.Error("Version 不应为空")
	}
	if len(m.Capabilities) == 0 {
		t.Error("Capabilities 不应为空")
	}
	t.Logf("resolveMetadata: id=%s version=%s caps=%v", m.ID, m.Version, m.Capabilities)
}

func TestDataDir_Fallback(t *testing.T) {
	// 三级回退：DBX_PLUGIN_DATA_DIR 优先
	t.Setenv("DBX_PLUGIN_DATA_DIR", filepath.Join(t.TempDir(), "a"))
	if got := dataDir(); !strings.HasSuffix(got, "a") {
		t.Errorf("dataDir = %q, 期望环境变量值", got)
	}
	t.Setenv("DBX_PLUGIN_DATA_DIR", "")
	t.Setenv("DBX_PLUGIN_SPACE", filepath.Join(t.TempDir(), "b"))
	if got := dataDir(); !strings.HasSuffix(got, "b") {
		t.Errorf("dataDir = %q, 期望 DBX_PLUGIN_SPACE 值", got)
	}
	t.Setenv("DBX_PLUGIN_SPACE", "")
	got := dataDir()
	if got == "" {
		t.Error("dataDir 不应为空（应回退到 UserConfigDir 或 ./data）")
	}
	// 回退路径应含插件 ID，避免与其他插件的数据混在一起
	if !strings.Contains(got, pluginID) && !strings.Contains(got, "data") {
		t.Errorf("回退路径应含插件 ID 或 data: %q", got)
	}
}

func TestWriteConfig_Atomic(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DBX_PLUGIN_DATA_DIR", dir)
	writeConfig(map[string]any{"dataDir": dir, "test": 123})

	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("config.json 未写入: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("config.json 不是合法 JSON: %v", err)
	}
	if m["test"] != float64(123) {
		t.Errorf("内容不一致: %v", m)
	}
	// 不应留下 .tmp（原子写完成后必须清理，否则会堆积垃圾文件）
	if _, err := os.Stat(filepath.Join(dir, "config.json.tmp")); err == nil {
		t.Error("留下了 config.json.tmp（原子写未清理临时文件）")
	}
}

// TestLoadConfig_NoFileNoPanic 验证配置文件缺失时不 panic。
//
// 首次启动必然没有 config.json，这必须是正常路径而非错误。
func TestLoadConfig_NoFileNoPanic(t *testing.T) {
	t.Setenv("DBX_PLUGIN_DATA_DIR", t.TempDir())
	loadConfig() // 不应 panic
}

// ---------------------------------------------------------------------------
// 错误映射
// ---------------------------------------------------------------------------

func TestEngineErr_Mapping(t *testing.T) {
	cases := []struct {
		err  error
		code int
	}{
		{engine.New(engine.KindAuth, "鉴权失败"), -32031},
		{engine.New(engine.KindRateLimit, "限流"), -32032},
		{engine.New(engine.KindProtocol, "协议错误"), -32033},
		{engine.New(engine.KindCancelled, "已取消"), -32034},
		{engine.New(engine.KindConfig, "配置错误"), -32602},
		{engine.New(engine.KindUnsupported, "不支持"), -32601},
		{engine.New(engine.KindNetwork, "网络错误"), -32030},
		{engine.New(engine.KindUpstream, "上游错误"), -32030},
		{errors.New("普通错误"), -32030}, // 未分类错误保守归为可重试的网络问题
	}
	for _, c := range cases {
		got := engineErr(c.err)
		if got.Code != c.code {
			t.Errorf("engineErr(%v) 的 code = %d, 期望 %d", c.err, got.Code, c.code)
		}
		if got.Message == "" {
			t.Errorf("engineErr(%v) 的消息不应为空", c.err)
		}
	}
	// nil 错误不应 panic
	if got := engineErr(nil); got == nil || got.Code != -32030 {
		t.Errorf("engineErr(nil) 应返回兜底错误，得到 %v", got)
	}
}

// TestEngineErr_RedactsSecrets 验证错误消息经过脱敏。
//
// 上游返回的错误体可能回显请求头（含 Authorization），
// 不脱敏就会把密钥写进用户可见的错误提示与诊断报告。
func TestEngineErr_RedactsSecrets(t *testing.T) {
	const fakeKey = "sk-leak-ABCDEFGH12345678"
	e := engine.New(engine.KindUpstream, "上游报错 apiKey="+fakeKey)
	got := engineErr(e)
	if strings.Contains(got.Message, fakeKey) {
		t.Errorf("错误消息泄露了密钥: %s", got.Message)
	}
}
