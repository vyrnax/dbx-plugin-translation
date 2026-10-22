package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// newTestStore 在临时目录里开一个干净的库。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// mkEngine 构造一个可用的测试引擎。
//
// 【Enabled 必须显式设为 true】：store.SaveEngine 忠实持久化调用方给的
// Enabled 值（不做隐式默认），因此新建引擎时由上层（main.go 的 RPC 层）
// 负责默认启用。测试helper 必须模拟这个契约，否则新建的引擎是禁用状态，
// DefaultEngine 会因为「没有启用的引擎」而返回 ErrNotFound。
func mkEngine(id, kind, name string) Engine {
	return Engine{
		ID:      id,
		Kind:    kind,
		Name:    name,
		Enabled: true,
		Config:  map[string]any{"endpoint": "https://api.example.com/v1"},
	}
}

// TestOpen_CreatesAndMigrates 验证首次打开会建库建表。
func TestOpen_CreatesAndMigrates(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = s.Close() }()

	if !fileExists(filepath.Join(dir, "translation.db")) {
		t.Error("未创建 translation.db")
	}
	// 五张表都应存在
	for _, tbl := range []string{"meta", "engines", "secrets", "history", "prefs"} {
		var n int
		err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&n)
		if err != nil || n != 1 {
			t.Errorf("表 %s 不存在 (n=%d, err=%v)", tbl, n, err)
		}
	}
	// meta 应已初始化 rev=0 与 schema_version
	rev, err := s.Rev()
	if err != nil {
		t.Fatalf("读 rev 失败: %v", err)
	}
	if rev != 0 {
		t.Errorf("初始 rev = %d, 期望 0", rev)
	}
	var sv string
	if err := s.db.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&sv); err != nil {
		t.Fatalf("读 schema_version 失败: %v", err)
	}
	if sv != schemaVersion {
		t.Errorf("schema_version = %q, 期望 %q", sv, schemaVersion)
	}
}

// TestOpen_MigrateIdempotent 验证重复打开同一库不会报错（迁移幂等）。
// 侧车每次启动都会走这条路径，迁移不幂等会导致「重启后插件坏了」。
func TestOpen_MigrateIdempotent(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("第 %d 次 Open 失败: %v", i+1, err)
		}
		if i == 0 {
			if _, err := s.SaveEngine(mkEngine("e1", "openai", "引擎一"), -1); err != nil {
				t.Fatalf("写入失败: %v", err)
			}
		}
		_ = s.Close()
	}
	// 第三次打开后数据应仍在
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("最终 Open 失败: %v", err)
	}
	defer func() { _ = s.Close() }()
	list, err := s.ListEngines()
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(list) != 1 || list[0].ID != "e1" {
		t.Errorf("数据丢失: %v", list)
	}
}

// TestOpen_EmptyDir 验证空目录被拒绝（避免把库建到根目录）。
func TestOpen_EmptyDir(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Error("空目录应被拒绝")
	}
	if _, err := Open("   "); err == nil {
		t.Error("纯空白目录应被拒绝")
	}
}

// TestSaveEngine_FirstBecomesDefault 验证第一个引擎自动成为默认。
// 用户不必手动点一次「设为默认」，这是体验细节。
func TestSaveEngine_FirstBecomesDefault(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.SaveEngine(mkEngine("e1", "openai", "引擎一"), -1); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	got, err := s.GetEngine("e1")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !got.IsDefault {
		t.Error("第一个引擎应自动成为默认")
	}
	// 加第二个，默认仍应是第一个
	if _, err := s.SaveEngine(mkEngine("e2", "deepl", "引擎二"), -1); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	def, err := s.DefaultEngine()
	if err != nil {
		t.Fatalf("读默认引擎失败: %v", err)
	}
	if def.ID != "e1" {
		t.Errorf("默认引擎 = %s, 期望 e1", def.ID)
	}
}

// TestSaveEngine_OnlyOneDefault 验证设默认时旧默认被清除。
// 两个「默认引擎」会让翻译时不知道用哪个。
func TestSaveEngine_OnlyOneDefault(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"e1", "e2", "e3"} {
		if _, err := s.SaveEngine(mkEngine(id, "openai", id), -1); err != nil {
			t.Fatalf("保存 %s 失败: %v", id, err)
		}
	}
	e2 := mkEngine("e2", "openai", "e2")
	e2.IsDefault = true
	if _, err := s.SaveEngine(e2, -1); err != nil {
		t.Fatalf("设默认失败: %v", err)
	}
	list, _ := s.ListEngines()
	defaults := 0
	for _, e := range list {
		if e.IsDefault {
			defaults++
		}
	}
	if defaults != 1 {
		t.Errorf("默认引擎数量 = %d, 期望恰好 1", defaults)
	}
}

// TestSaveEngine_RevIncrements 验证每次写入递增 rev。
func TestSaveEngine_RevIncrements(t *testing.T) {
	s := newTestStore(t)
	r0, _ := s.Rev()
	r1, err := s.SaveEngine(mkEngine("e1", "openai", "一"), r0)
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if r1 != r0+1 {
		t.Errorf("rev = %d, 期望 %d", r1, r0+1)
	}
	r2, err := s.SaveEngine(mkEngine("e2", "deepl", "二"), r1)
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if r2 != r1+1 {
		t.Errorf("rev = %d, 期望 %d", r2, r1+1)
	}
}

// TestSaveEngine_Conflict 验证乐观锁：旧 rev 被拒绝且不覆盖数据。
// 这是「多窗口同时编辑」时不丢数据的唯一保障。
func TestSaveEngine_Conflict(t *testing.T) {
	s := newTestStore(t)
	r0, _ := s.Rev()
	r1, _ := s.SaveEngine(mkEngine("e1", "openai", "原名"), r0)

	// 用旧 rev（r0）再写一次 → 必须冲突
	stale := mkEngine("e1", "openai", "被覆盖的名字")
	_, err := s.SaveEngine(stale, r0)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("旧 rev 应返回 ErrConflict，得到 %v", err)
	}
	// 数据必须保持原样，绝不能被覆盖
	got, _ := s.GetEngine("e1")
	if got.Name != "原名" {
		t.Errorf("冲突时数据被覆盖了！name = %q, 期望 原名", got.Name)
	}
	// 用正确 rev 应成功
	r2, err := s.SaveEngine(stale, r1)
	if err != nil {
		t.Fatalf("正确 rev 应成功，得到 %v", err)
	}
	if r2 != r1+1 {
		t.Errorf("rev = %d, 期望 %d", r2, r1+1)
	}
	got, _ = s.GetEngine("e1")
	if got.Name != "被覆盖的名字" {
		t.Errorf("更新未生效: %q", got.Name)
	}
}

// TestSaveEngine_SkipRevCheck 验证 expectRev<0 时跳过校验（导入场景）。
func TestSaveEngine_SkipRevCheck(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.SaveEngine(mkEngine("e1", "openai", "一"), -1); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	// 用完全错误的 rev 但传 -1 → 应成功
	if _, err := s.SaveEngine(mkEngine("e2", "deepl", "二"), -1); err != nil {
		t.Fatalf("expectRev=-1 应跳过校验，得到 %v", err)
	}
}

// TestSaveEngine_Validation 验证必填字段校验。
func TestSaveEngine_Validation(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.SaveEngine(Engine{Kind: "openai"}, -1); err == nil {
		t.Error("空 ID 应被拒绝")
	}
	if _, err := s.SaveEngine(Engine{ID: "e1"}, -1); err == nil {
		t.Error("空 Kind 应被拒绝")
	}
	// 空名称应用 Kind 兜底，而不是报错
	if _, err := s.SaveEngine(Engine{ID: "e1", Kind: "openai"}, -1); err != nil {
		t.Fatalf("空名称应兜底而非报错: %v", err)
	}
	got, _ := s.GetEngine("e1")
	if got.Name != "openai" {
		t.Errorf("空名称应兜底为 Kind，得到 %q", got.Name)
	}
}

// TestSaveEngine_ConfigRoundTrip 验证配置 map 的序列化往返。
func TestSaveEngine_ConfigRoundTrip(t *testing.T) {
	s := newTestStore(t)
	e := mkEngine("e1", "openai", "一")
	e.Config = map[string]any{
		"endpoint":      "https://api.deepseek.com/v1/chat/completions",
		"model":         "deepseek-chat",
		"temperature":   0.3,
		"maxChars":      20000,
		"skipTlsVerify": false,
		"nested":        map[string]any{"a": 1, "b": "x"},
	}
	if _, err := s.SaveEngine(e, -1); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	got, err := s.GetEngine("e1")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.Config["endpoint"] != e.Config["endpoint"] {
		t.Errorf("endpoint 丢失: %v", got.Config["endpoint"])
	}
	if got.Config["model"] != "deepseek-chat" {
		t.Errorf("model 丢失: %v", got.Config["model"])
	}
	// JSON 往返后数字变成 float64，这是预期行为
	if v, ok := got.Config["temperature"].(float64); !ok || v != 0.3 {
		t.Errorf("temperature 丢失或类型错误: %v (%T)", got.Config["temperature"], got.Config["temperature"])
	}
	if _, ok := got.Config["nested"].(map[string]any); !ok {
		t.Errorf("嵌套对象丢失: %v", got.Config["nested"])
	}
}

// TestSaveEngine_NilConfig 验证 nil 配置不 panic 且读回为空 map。
func TestSaveEngine_NilConfig(t *testing.T) {
	s := newTestStore(t)
	e := Engine{ID: "e1", Kind: "openai", Name: "一", Config: nil}
	if _, err := s.SaveEngine(e, -1); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	got, _ := s.GetEngine("e1")
	if got.Config == nil {
		t.Error("nil 配置应读回为空 map 而非 nil（前端可直接遍历）")
	}
}

// TestGetEngine_NotFound 验证不存在的 ID 返回 ErrNotFound。
func TestGetEngine_NotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetEngine("不存在")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("应返回 ErrNotFound，得到 %v", err)
	}
}

// TestDeleteEngine_CascadesSecret 验证删除引擎时密钥一起删（不留孤儿记录）。
func TestDeleteEngine_CascadesSecret(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.SaveEngine(mkEngine("e1", "openai", "一"), -1); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if err := s.SaveSecret("e1", "cipher-text", "dpapi", "sk-t****7890"); err != nil {
		t.Fatalf("保存密钥失败: %v", err)
	}
	if _, err := s.DeleteEngine("e1", -1); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, err := s.GetSecret("e1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("密钥应随引擎一起删除，得到 %v", err)
	}
	n, _ := s.CountSecrets()
	if n != 0 {
		t.Errorf("secrets 表应清空，剩余 %d 条", n)
	}
}

// TestDeleteEngine_NotFound 验证删除不存在的引擎返回 ErrNotFound 且不递增 rev。
func TestDeleteEngine_NotFound(t *testing.T) {
	s := newTestStore(t)
	r0, _ := s.Rev()
	_, err := s.DeleteEngine("不存在", -1)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("应返回 ErrNotFound，得到 %v", err)
	}
	r1, _ := s.Rev()
	if r1 != r0 {
		t.Errorf("删除不存在的引擎不应递增 rev: %d -> %d", r0, r1)
	}
}

// TestDeleteEngine_PromotesNewDefault 验证删掉默认引擎后自动提升新默认。
// 否则用户会发现「没有默认引擎了」，翻译按钮不知道用哪个。
func TestDeleteEngine_PromotesNewDefault(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"e1", "e2"} {
		if _, err := s.SaveEngine(mkEngine(id, "openai", id), -1); err != nil {
			t.Fatalf("保存失败: %v", err)
		}
	}
	if _, err := s.DeleteEngine("e1", -1); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	def, err := s.DefaultEngine()
	if err != nil {
		t.Fatalf("应仍有默认引擎，得到 %v", err)
	}
	if def.ID != "e2" {
		t.Errorf("新默认引擎 = %s, 期望 e2", def.ID)
	}
}

// TestDefaultEngine_FallbackToEnabled 验证默认引擎被禁用时回退到第一个启用的。
func TestDefaultEngine_FallbackToEnabled(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"e1", "e2"} {
		if _, err := s.SaveEngine(mkEngine(id, "openai", id), -1); err != nil {
			t.Fatalf("保存失败: %v", err)
		}
	}
	// 禁用默认引擎 e1
	if _, err := s.SetEngineEnabled("e1", false, -1); err != nil {
		t.Fatalf("禁用失败: %v", err)
	}
	def, err := s.DefaultEngine()
	if err != nil {
		t.Fatalf("应回退到启用的引擎，得到 %v", err)
	}
	if def.ID != "e2" {
		t.Errorf("回退引擎 = %s, 期望 e2", def.ID)
	}
	// 全部禁用 → ErrNotFound
	if _, err := s.SetEngineEnabled("e2", false, -1); err != nil {
		t.Fatalf("禁用失败: %v", err)
	}
	if _, err := s.DefaultEngine(); !errors.Is(err, ErrNotFound) {
		t.Errorf("全部禁用时应返回 ErrNotFound，得到 %v", err)
	}
}

// TestReorderEngines 验证排序生效且未列出的引擎不消失。
func TestReorderEngines(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"e1", "e2", "e3"} {
		if _, err := s.SaveEngine(mkEngine(id, "openai", id), -1); err != nil {
			t.Fatalf("保存失败: %v", err)
		}
	}
	// 只列出两个，e3 应排到后面而不是消失
	if _, err := s.ReorderEngines([]string{"e2", "e1"}, -1); err != nil {
		t.Fatalf("重排失败: %v", err)
	}
	list, _ := s.ListEngines()
	if len(list) != 3 {
		t.Fatalf("引擎数量 = %d, 期望 3（未列出的不应消失）", len(list))
	}
	if list[0].ID != "e2" || list[1].ID != "e1" {
		t.Errorf("顺序错误: %s, %s, %s", list[0].ID, list[1].ID, list[2].ID)
	}
	if list[2].ID != "e3" {
		t.Errorf("未列出的引擎应排在最后，得到 %s", list[2].ID)
	}
}

// TestSecrets_RoundTrip 验证密钥密文存取。
func TestSecrets_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.SaveEngine(mkEngine("e1", "openai", "一"), -1); err != nil {
		t.Fatalf("保存引擎失败: %v", err)
	}
	const cipherText = "AQAAANCMnd8BFdERjHoAwE/Cl+sBAAAA"
	if err := s.SaveSecret("e1", cipherText, "dpapi", "sk-t****7890"); err != nil {
		t.Fatalf("保存密钥失败: %v", err)
	}
	rec, err := s.GetSecret("e1")
	if err != nil {
		t.Fatalf("读取密钥失败: %v", err)
	}
	if rec.Cipher != cipherText || rec.Scheme != "dpapi" {
		t.Errorf("密钥记录不一致: %+v", rec)
	}
	// engines 表的元信息应同步
	e, _ := s.GetEngine("e1")
	if !e.HasSecret {
		t.Error("HasSecret 应为 true")
	}
	if e.Fingerprint != "sk-t****7890" {
		t.Errorf("Fingerprint = %q", e.Fingerprint)
	}
	// 覆盖写入应更新而非新增
	if err := s.SaveSecret("e1", "new-cipher", "aesgcm-machine", "new-****ger"); err != nil {
		t.Fatalf("覆盖密钥失败: %v", err)
	}
	n, _ := s.CountSecrets()
	if n != 1 {
		t.Errorf("密钥记录数 = %d, 期望 1（覆盖不应新增）", n)
	}
	rec, _ = s.GetSecret("e1")
	if rec.Cipher != "new-cipher" || rec.Scheme != "aesgcm-machine" {
		t.Errorf("覆盖未生效: %+v", rec)
	}
}

// TestSecrets_Validation 验证密钥保存的必填校验与孤儿防护。
func TestSecrets_Validation(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveSecret("", "c", "dpapi", "f"); err == nil {
		t.Error("空引擎 ID 应被拒绝")
	}
	if err := s.SaveSecret("e1", "", "dpapi", "f"); err == nil {
		t.Error("空密文应被拒绝")
	}
	if err := s.SaveSecret("e1", "c", "", "f"); err == nil {
		t.Error("空方案标识应被拒绝")
	}
	// 给不存在的引擎存密钥应失败（防孤儿记录）
	if err := s.SaveSecret("不存在的引擎", "c", "dpapi", "f"); !errors.Is(err, ErrNotFound) {
		t.Errorf("应返回 ErrNotFound，得到 %v", err)
	}
}

// TestSecrets_ListDoesNotExposeCipher 是关键安全断言：
// 引擎列表查询绝不能带出密文。
//
// 这是结构性的安全保证 —— ListEngines 刻意不 JOIN secrets 表，
// 因此即使有人把返回的 []Engine 整个序列化到日志或 RPC 响应里，
// 也不可能泄露密钥。
func TestSecrets_ListDoesNotExposeCipher(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.SaveEngine(mkEngine("e1", "openai", "一"), -1); err != nil {
		t.Fatalf("保存引擎失败: %v", err)
	}
	const cipherText = "SUPER-SECRET-CIPHERTEXT-12345"
	if err := s.SaveSecret("e1", cipherText, "dpapi", "sk-t****7890"); err != nil {
		t.Fatalf("保存密钥失败: %v", err)
	}
	list, err := s.ListEngines()
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	// 把整个列表序列化成 JSON，检查密文是否泄露
	blob := fmt.Sprintf("%+v", list)
	if contains(blob, cipherText) {
		t.Errorf("引擎列表泄露了密文！\n%v", list)
	}
	// Engine 结构体里根本不该有能装密文的字段
	for _, e := range list {
		if !e.HasSecret {
			t.Error("HasSecret 应为 true")
		}
		if e.Fingerprint == cipherText {
			t.Error("Fingerprint 不应是密文")
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		})()
}

// TestDeleteSecret 验证删除密钥并清除标记。
func TestDeleteSecret(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.SaveEngine(mkEngine("e1", "openai", "一"), -1); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if err := s.SaveSecret("e1", "c", "dpapi", "f"); err != nil {
		t.Fatalf("保存密钥失败: %v", err)
	}
	if err := s.DeleteSecret("e1"); err != nil {
		t.Fatalf("删除密钥失败: %v", err)
	}
	if _, err := s.GetSecret("e1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("密钥应已删除，得到 %v", err)
	}
	e, _ := s.GetEngine("e1")
	if e.HasSecret {
		t.Error("HasSecret 应被清除")
	}
	if e.Fingerprint != "" {
		t.Errorf("Fingerprint 应被清除，得到 %q", e.Fingerprint)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// TestConcurrentWrites 验证并发写入不丢数据、不死锁（配合 -race 跑）。
//
// SetMaxOpenConns(1) + WithTx 的互斥锁共同保证串行化。
// 这个测试是「多窗口同时操作」场景的回归防线。
func TestConcurrentWrites(t *testing.T) {
	s := newTestStore(t)
	const n = 30
	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// expectRev=-1 跳过乐观锁：这里测的是底层并发安全，
			// 不是业务层的冲突检测（那个由 TestSaveEngine_Conflict 覆盖）
			if _, err := s.SaveEngine(mkEngine(fmt.Sprintf("e%d", i), "openai", fmt.Sprintf("引擎%d", i)), -1); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("并发写入失败: %v", err)
	}
	list, err := s.ListEngines()
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(list) != n {
		t.Errorf("并发写入后引擎数 = %d, 期望 %d（有数据丢失）", len(list), n)
	}
}

// TestConcurrentHistoryWrites 验证历史并发追加不丢数据。
func TestConcurrentHistoryWrites(t *testing.T) {
	s := newTestStore(t)
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = s.AddHistory(HistoryItem{
				ID:     fmt.Sprintf("h%d", i),
				Source: fmt.Sprintf("源文 %d", i),
				Target: fmt.Sprintf("译文 %d", i),
				Chars:  i,
			}, 0) // limit=0 表示不淘汰
		}(i)
	}
	wg.Wait()
	total, err := s.CountHistory()
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if total != n {
		t.Errorf("历史条数 = %d, 期望 %d", total, n)
	}
}
