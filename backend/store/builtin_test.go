package store

import (
	"errors"
	"testing"
)

// mustExecForTest 只为测试而存在：直接跑一条 SQL 把库置于异常状态。
//
// 为什么定义在 _test.go 里而不是给 Store 加一个导出方法：
// 生产代码没有任何理由需要裸执行 SQL，暴露这样一个口子等于给未来
// 「顺手绕过一层防护」留了门。测试文件里的方法不会进编译产物。
func mustExecForTest(t *testing.T, s *Store, query string) {
	t.Helper()
	if _, err := s.db.Exec(query); err != nil {
		t.Fatalf("执行测试语句失败: %v", err)
	}
}

// builtinTplForTest 是测试用的内置引擎模板。
//
// 真实模板由 main.go 用 engine/free 的常量拼出来；store 是纯持久层，
// 不该知道任何引擎 kind，所以这里给一份等价的假模板来验证保护规则本身。
func builtinTplForTest() Engine {
	return Engine{
		ID:      BuiltinEngineID,
		Kind:    "free",
		Name:    "免费翻译服务",
		Enabled: true,
		Builtin: true,
		Config:  map[string]any{"endpoint": "https://free.example.com/v1", "maxChars": 400},
	}
}

// TestEnsureBuiltin_FirstInstallIsDefault 验证全新安装时内置引擎直接当默认。
//
// 这条决定「装好插件第一次打开就能翻译」是否成立，是内置引擎存在的意义。
func TestEnsureBuiltin_FirstInstallIsDefault(t *testing.T) {
	s := newTestStore(t)
	created, err := s.EnsureBuiltin(builtinTplForTest())
	if err != nil {
		t.Fatalf("EnsureBuiltin 失败: %v", err)
	}
	if !created {
		t.Error("首次调用应当报告 created=true")
	}
	e, err := s.GetEngine(BuiltinEngineID)
	if err != nil {
		t.Fatalf("读取内置引擎失败: %v", err)
	}
	if !e.Builtin {
		t.Error("builtin 标记没落库（前端的隐藏编辑/删除全靠它）")
	}
	if !e.IsDefault {
		t.Error("空库里内置引擎应当是默认引擎")
	}
	if !e.Enabled {
		t.Error("内置引擎应当默认启用")
	}
	if def, err := s.DefaultEngine(); err != nil || def.ID != BuiltinEngineID {
		t.Errorf("DefaultEngine() = %+v, err=%v, 期望内置引擎", def, err)
	}
}

// TestEnsureBuiltin_IdempotentAndNoRevChurn 验证重复调用不写库、不递增 rev。
//
// rev 不递增是有意的：侧车每次启动都跑 EnsureBuiltin，
// 若无脑 UPDATE 就会每次 bumpRev，另一个 DBX 窗口手里的 rev 立刻过期，
// 用户会看到莫名其妙的「配置已被其它会话修改」并触发一次冲突重试。
func TestEnsureBuiltin_IdempotentAndNoRevChurn(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.EnsureBuiltin(builtinTplForTest()); err != nil {
		t.Fatalf("首次 EnsureBuiltin 失败: %v", err)
	}
	rev1, err := s.Rev()
	if err != nil {
		t.Fatalf("读 rev 失败: %v", err)
	}
	created, err := s.EnsureBuiltin(builtinTplForTest())
	if err != nil {
		t.Fatalf("二次 EnsureBuiltin 失败: %v", err)
	}
	if created {
		t.Error("二次调用不应报告 created")
	}
	rev2, _ := s.Rev()
	if rev2 != rev1 {
		t.Errorf("重复 EnsureBuiltin 递增了 rev: %d → %d（会让其它窗口的乐观锁白白失效）", rev1, rev2)
	}
	list, _ := s.ListEngines()
	if len(list) != 1 {
		t.Errorf("内置引擎被重复播种了，当前 %d 条记录", len(list))
	}
}

// TestEnsureBuiltin_RefreshesCodeOwnedFields 验证升级时名字/类型/配置会被刷新，
// 而用户拥有的 enabled / 默认 / 排序一概不动。
func TestEnsureBuiltin_RefreshesCodeOwnedFields(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.EnsureBuiltin(builtinTplForTest()); err != nil {
		t.Fatalf("EnsureBuiltin 失败: %v", err)
	}
	// 模拟用户操作：禁用内置引擎
	if _, err := s.SetEngineEnabled(BuiltinEngineID, false, -1); err != nil {
		t.Fatalf("禁用失败: %v", err)
	}
	// 模拟插件升级：代码里的端点变了
	tpl := builtinTplForTest()
	tpl.Name = "免费翻译服务(新)"
	tpl.Config = map[string]any{"endpoint": "https://new.example.com/v1", "maxChars": 500}
	if _, err := s.EnsureBuiltin(tpl); err != nil {
		t.Fatalf("升级后 EnsureBuiltin 失败: %v", err)
	}
	e, err := s.GetEngine(BuiltinEngineID)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if e.Name != "免费翻译服务(新)" {
		t.Errorf("name 未随代码刷新: %q（升级换了模型/端点会被旧记录锁死在旧服务端）", e.Name)
	}
	if got, _ := e.Config["endpoint"].(string); got != "https://new.example.com/v1" {
		t.Errorf("config 未随代码刷新: %v", e.Config)
	}
	if e.Enabled {
		t.Error("用户的禁用选择被覆盖了 —— enabled 属于用户，EnsureBuiltin 不该动它")
	}
}

// TestEnsureBuiltin_DoesNotStealUserDefault 验证已有用户引擎时不抢默认。
func TestEnsureBuiltin_DoesNotStealUserDefault(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.SaveEngine(mkEngine("u1", "openai", "我的引擎"), -1); err != nil {
		t.Fatalf("保存用户引擎失败: %v", err)
	}
	if _, err := s.EnsureBuiltin(builtinTplForTest()); err != nil {
		t.Fatalf("EnsureBuiltin 失败: %v", err)
	}
	e, err := s.GetEngine(BuiltinEngineID)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if e.IsDefault {
		t.Error("用户已经有默认引擎时，内置引擎不该把它抢走")
	}
	if def, _ := s.DefaultEngine(); def.ID != "u1" {
		t.Errorf("DefaultEngine 变成了 %s, 期望仍是用户的 u1", def.ID)
	}
}

// TestBuiltin_DeleteRejected 验证内置引擎不可删除。
func TestBuiltin_DeleteRejected(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.EnsureBuiltin(builtinTplForTest()); err != nil {
		t.Fatalf("EnsureBuiltin 失败: %v", err)
	}
	revBefore, _ := s.Rev()
	_, err := s.DeleteEngine(BuiltinEngineID, -1)
	if !errors.Is(err, ErrBuiltin) {
		t.Fatalf("删除内置引擎应当返回 ErrBuiltin，实际: %v", err)
	}
	revAfter, _ := s.Rev()
	if revAfter != revBefore {
		t.Errorf("被拒绝的删除递增了 rev（%d → %d），会白白让其它会话冲突一次", revBefore, revAfter)
	}
	if _, err := s.GetEngine(BuiltinEngineID); err != nil {
		t.Errorf("内置引擎被删掉了: %v", err)
	}
}

// TestBuiltin_SaveCannotRewriteIdentity 验证通过 SaveEngine 也改不动内置记录。
//
// 这是【存储层】防线，防的是绕过 UI 的写入：data/import、
// 手工构造的 RPC、以及未来任何新增的写入路径。
// 只在 handler 里判断的话，漏一条路径就等于「内置引擎被改名/改端点」。
func TestBuiltin_SaveCannotRewriteIdentity(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.EnsureBuiltin(builtinTplForTest()); err != nil {
		t.Fatalf("EnsureBuiltin 失败: %v", err)
	}
	before, _ := s.GetEngine(BuiltinEngineID)

	evil := Engine{
		ID:      BuiltinEngineID,
		Kind:    "openai",
		Name:    "被我改名的内置引擎",
		Enabled: true,
		Config:  map[string]any{"endpoint": "https://evil.example.com/v1"},
	}
	if _, err := s.SaveEngine(evil, -1); err != nil {
		t.Fatalf("SaveEngine 失败: %v", err)
	}
	after, _ := s.GetEngine(BuiltinEngineID)
	if after.Name != before.Name {
		t.Errorf("内置引擎的名字被改写: %q → %q", before.Name, after.Name)
	}
	if after.Kind != before.Kind {
		t.Errorf("内置引擎的类型被改写: %q → %q", before.Kind, after.Kind)
	}
	if got, _ := after.Config["endpoint"].(string); got != "https://free.example.com/v1" {
		t.Errorf("内置引擎的端点被改写: %v", after.Config)
	}
	if !after.Builtin {
		t.Error("builtin 标记被抹掉了 —— 这意味着删除保护也一起失效")
	}

	// 但用户确实需要的三项必须可改：enabled / 默认 / 排序
	if _, err := s.SetEngineEnabled(BuiltinEngineID, false, -1); err != nil {
		t.Fatalf("禁用内置引擎失败: %v", err)
	}
	if e, _ := s.GetEngine(BuiltinEngineID); e.Enabled {
		t.Error("禁用未生效")
	}
}

// TestBuiltin_FirstUserEngineBecomesDefault 验证用户配好第一个官方引擎后，
// 默认引擎从内置切到它。
//
// 这是内置引擎引入的一个【必须显式断言】的行为变化：
// 旧的自动设默认判据是「库里只有 1 条记录」，而内置记录永久占着一条，
// 判据不改造成的症状是「用户专门配好了官方引擎，翻译却还在走免费端点」。
func TestBuiltin_FirstUserEngineBecomesDefault(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.EnsureBuiltin(builtinTplForTest()); err != nil {
		t.Fatalf("EnsureBuiltin 失败: %v", err)
	}
	if _, err := s.SaveEngine(mkEngine("u1", "deepl", "我的 DeepL"), -1); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	def, err := s.DefaultEngine()
	if err != nil {
		t.Fatalf("DefaultEngine 失败: %v", err)
	}
	if def.ID != "u1" {
		t.Errorf("默认引擎 = %s, 期望首个用户引擎 u1", def.ID)
	}
	// 内置引擎必须退位而不是被删
	b, _ := s.GetEngine(BuiltinEngineID)
	if b.IsDefault {
		t.Error("内置引擎仍占着默认标记（会出现两个默认引擎）")
	}
	// 第二个用户引擎不应再抢默认
	if _, err := s.SaveEngine(mkEngine("u2", "baidu", "百度"), -1); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if def, _ := s.DefaultEngine(); def.ID != "u1" {
		t.Errorf("第二个引擎抢走了默认: %s, 期望仍是 u1", def.ID)
	}
}

// TestEnsureBuiltin_PromotesWhenNoDefault 验证「没有默认引擎」时内置引擎兜底。
//
// 触发路径：用户删掉了默认引擎，而删除时的提升逻辑因并发/异常没兜住 ——
// 这时翻译页会「没有可用引擎」，内置引擎必须把它接住。
func TestEnsureBuiltin_PromotesWhenNoDefault(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.SaveEngine(mkEngine("u1", "openai", "我的引擎"), -1); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if _, err := s.EnsureBuiltin(builtinTplForTest()); err != nil {
		t.Fatalf("EnsureBuiltin 失败: %v", err)
	}
	// 手工把默认标记全部清掉，模拟异常状态（正常 RPC 路径走不出这个状态）
	mustExecForTest(t, s, `UPDATE engines SET is_default=0`)
	if _, err := s.EnsureBuiltin(builtinTplForTest()); err != nil {
		t.Fatalf("EnsureBuiltin 失败: %v", err)
	}
	if def, err := s.DefaultEngine(); err != nil || def.ID != BuiltinEngineID {
		t.Errorf("兜底后 DefaultEngine() = %+v err=%v, 期望内置引擎", def, err)
	}
}

// TestBuiltin_ListOrdering 验证内置引擎与其它引擎一样只按 ord 排序，
// 且「设为默认」不让它插队 —— 顺序与默认彻底解耦。
func TestBuiltin_ListOrdering(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"u1", "u2"} {
		if _, err := s.SaveEngine(mkEngine(id, "openai", id), -1); err != nil {
			t.Fatalf("保存失败: %v", err)
		}
	}
	if _, err := s.EnsureBuiltin(builtinTplForTest()); err != nil {
		t.Fatalf("EnsureBuiltin 失败: %v", err)
	}
	ids := func() string {
		list, err := s.ListEngines()
		if err != nil {
			t.Fatalf("列表查询失败: %v", err)
		}
		if len(list) != 3 {
			t.Fatalf("引擎数量 = %d, 期望 3", len(list))
		}
		return list[0].ID + "," + list[1].ID + "," + list[2].ID
	}

	// 已有用户引擎时播种的内置引擎排在末尾（EnsureBuiltin 取 max(ord)+1），且不抢默认位。
	before := ids()
	if before != "u1,u2,builtin.free" {
		t.Errorf("初始顺序 = %s, 期望 u1,u2,builtin.free", before)
	}
	if e, _ := s.GetEngine(BuiltinEngineID); e.IsDefault {
		t.Error("已有用户引擎时，内置引擎不该抢走默认")
	}

	// 把内置引擎设为默认：标记要变，顺序【不能】变。
	if _, err := s.SetDefaultEngine(BuiltinEngineID, -1); err != nil {
		t.Fatalf("设默认失败: %v", err)
	}
	if got := ids(); got != before {
		t.Errorf("设默认后顺序变成 %s, 期望保持 %s（设默认不是重新排版的指令）", got, before)
	}
	if def, err := s.DefaultEngine(); err != nil || def.ID != BuiltinEngineID {
		t.Errorf("DefaultEngine() = %+v err=%v, 期望内置引擎", def, err)
	}

	// 用户也可以亲手把内置引擎拖到第一位 —— 与它是不是默认无关。
	if _, err := s.ReorderEngines([]string{BuiltinEngineID, "u2", "u1"}, -1); err != nil {
		t.Fatalf("重排失败: %v", err)
	}
	if got := ids(); got != "builtin.free,u2,u1" {
		t.Errorf("重排后的顺序 = %s, 期望 builtin.free,u2,u1", got)
	}
}
