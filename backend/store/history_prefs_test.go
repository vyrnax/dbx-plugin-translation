package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"
)

// numEq 比较 JSON 往返后的数值。
//
// 偏好存库要经 json.Marshal/Unmarshal，Go 的 encoding/json 会把所有数字
// 统一解成 float64。因此 `prefs["x"] != 1200` 这种写法【永远为真】——
// 接口比较时 float64(1200) 与 int(1200) 是不同类型，不相等。
// 必须先把两边都转成 float64 再比。
func numEq(v any, want int) bool {
	switch n := v.(type) {
	case float64:
		return n == float64(want)
	case int:
		return n == want
	case int64:
		return n == int64(want)
	case json.Number:
		f, err := n.Float64()
		return err == nil && f == float64(want)
	}
	return false
}

// boolEq 比较 JSON 往返后的布尔值（布尔不会变类型，但统一风格便于阅读）。
func boolEq(v any, want bool) bool {
	b, ok := v.(bool)
	return ok && b == want
}

func mkHistory(id, src, dst string, starred bool, createdAt string) HistoryItem {
	return HistoryItem{
		ID:         id,
		EngineID:   "e1",
		EngineName: "引擎一",
		EngineKind: "openai",
		SrcLang:    "en",
		DstLang:    "zh",
		Source:     src,
		Target:     dst,
		Chars:      len([]rune(src)),
		ElapsedMs:  120,
		Chunks:     1,
		Starred:    starred,
		CreatedAt:  createdAt,
	}
}

// TestAddHistory_Basic 验证历史写入与读回。
func TestAddHistory_Basic(t *testing.T) {
	s := newTestStore(t)
	if err := s.AddHistory(mkHistory("h1", "hello", "你好", false, ""), 0); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	items, total, err := s.ListHistory(HistoryQuery{})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("total=%d len=%d, 期望 1/1", total, len(items))
	}
	got := items[0]
	if got.ID != "h1" || got.Source != "hello" || got.Target != "你好" {
		t.Errorf("记录不一致: %+v", got)
	}
	if got.EngineName != "引擎一" || got.SrcLang != "en" || got.DstLang != "zh" {
		t.Errorf("元信息丢失: %+v", got)
	}
	if got.CreatedAt == "" {
		t.Error("CreatedAt 应自动填充")
	}
}

// TestAddHistory_Validation 验证必填校验。
func TestAddHistory_Validation(t *testing.T) {
	s := newTestStore(t)
	if err := s.AddHistory(HistoryItem{Source: "a", Target: "b"}, 0); err == nil {
		t.Error("空 ID 应被拒绝")
	}
}

// TestListHistory_Order 验证按创建时间倒序（最新的在前）。
func TestListHistory_Order(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		ts := base.Add(time.Duration(i) * time.Hour).Format(time.RFC3339)
		if err := s.AddHistory(mkHistory(fmt.Sprintf("h%d", i), fmt.Sprintf("src%d", i), "t", false, ts), 0); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}
	items, total, err := s.ListHistory(HistoryQuery{})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if total != 5 {
		t.Fatalf("total=%d, 期望 5", total)
	}
	// 倒序：h4 应在最前
	if items[0].ID != "h4" {
		t.Errorf("首条 = %s, 期望 h4（应按时间倒序）", items[0].ID)
	}
	if items[4].ID != "h0" {
		t.Errorf("末条 = %s, 期望 h0", items[4].ID)
	}
}

// TestListHistory_Pagination 验证分页：limit/offset 与 total 的关系。
func TestListHistory_Pagination(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 25; i++ {
		ts := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		if err := s.AddHistory(mkHistory(fmt.Sprintf("h%02d", i), "s", "t", false, ts), 0); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}
	// 第一页
	p1, total, err := s.ListHistory(HistoryQuery{Limit: 10, Offset: 0})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if total != 25 {
		t.Errorf("total=%d, 期望 25（分页不应改变总数）", total)
	}
	if len(p1) != 10 {
		t.Errorf("第一页 %d 条, 期望 10", len(p1))
	}
	// 第二页不应与第一页重叠
	p2, _, _ := s.ListHistory(HistoryQuery{Limit: 10, Offset: 10})
	if len(p2) != 10 {
		t.Errorf("第二页 %d 条, 期望 10", len(p2))
	}
	seen := map[string]bool{}
	for _, it := range p1 {
		seen[it.ID] = true
	}
	for _, it := range p2 {
		if seen[it.ID] {
			t.Errorf("分页重叠: %s 同时出现在两页", it.ID)
		}
	}
	// 最后一页只有 5 条
	p3, _, _ := s.ListHistory(HistoryQuery{Limit: 10, Offset: 20})
	if len(p3) != 5 {
		t.Errorf("末页 %d 条, 期望 5", len(p3))
	}
	// 超出范围的 offset 返回空
	p4, total4, _ := s.ListHistory(HistoryQuery{Limit: 10, Offset: 100})
	if len(p4) != 0 {
		t.Errorf("越界 offset 应返回空，得到 %d 条", len(p4))
	}
	if total4 != 25 {
		t.Errorf("越界 offset 时 total 仍应是 25，得到 %d", total4)
	}
}

// TestListHistory_LimitClamped 验证 limit 被夹到合理范围。
// 前端传入离谱的 limit 不应拖垮查询。
func TestListHistory_LimitClamped(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 3; i++ {
		_ = s.AddHistory(mkHistory(fmt.Sprintf("h%d", i), "s", "t", false, ""), 0)
	}
	// limit<=0 用默认值，不报错
	if _, _, err := s.ListHistory(HistoryQuery{Limit: 0}); err != nil {
		t.Errorf("limit=0 应用默认值而非报错: %v", err)
	}
	if _, _, err := s.ListHistory(HistoryQuery{Limit: -5}); err != nil {
		t.Errorf("负 limit 应被纠正而非报错: %v", err)
	}
	// 超大 limit 被夹到 500
	items, _, err := s.ListHistory(HistoryQuery{Limit: 100000})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(items) != 3 {
		t.Errorf("应返回全部 3 条，得到 %d", len(items))
	}
	// 负 offset 被纠正
	if _, _, err := s.ListHistory(HistoryQuery{Offset: -10}); err != nil {
		t.Errorf("负 offset 应被纠正而非报错: %v", err)
	}
}

// TestListHistory_Search 验证模糊搜索源文与译文。
func TestListHistory_Search(t *testing.T) {
	s := newTestStore(t)
	_ = s.AddHistory(mkHistory("h1", "hello world", "你好世界", false, ""), 0)
	_ = s.AddHistory(mkHistory("h2", "goodbye", "再见", false, ""), 0)
	_ = s.AddHistory(mkHistory("h3", "apple", "苹果", false, ""), 0)

	// 搜源文
	_, total, err := s.ListHistory(HistoryQuery{Q: "hello"})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if total != 1 {
		t.Errorf("搜 hello 命中 %d 条, 期望 1", total)
	}
	// 搜译文
	_, total, _ = s.ListHistory(HistoryQuery{Q: "再见"})
	if total != 1 {
		t.Errorf("搜 再见 命中 %d 条, 期望 1", total)
	}
	// 搜不存在的
	_, total, _ = s.ListHistory(HistoryQuery{Q: "不存在的内容"})
	if total != 0 {
		t.Errorf("搜不存在内容命中 %d 条, 期望 0", total)
	}
}

// TestListHistory_SearchEscapesWildcards 验证 LIKE 通配符被转义。
//
// 不转义的话，用户搜 "100%" 会把 % 当通配符，搜出所有含 "100" 的记录，
// 这与用户意图不符。
func TestListHistory_SearchEscapesWildcards(t *testing.T) {
	s := newTestStore(t)
	_ = s.AddHistory(mkHistory("h1", "进度 100% 完成", "progress", false, ""), 0)
	_ = s.AddHistory(mkHistory("h2", "进度 100 完成", "progress", false, ""), 0)

	// 搜 "100%" 应只命中字面含百分号的那条
	_, total, err := s.ListHistory(HistoryQuery{Q: "100%"})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if total != 1 {
		t.Errorf("搜 '100%%' 命中 %d 条, 期望 1（%% 应被当字面量）", total)
	}
	// 搜 "_" 同理
	_ = s.AddHistory(mkHistory("h3", "a_b", "t", false, ""), 0)
	_, total, _ = s.ListHistory(HistoryQuery{Q: "a_b"})
	if total != 1 {
		t.Errorf("搜 'a_b' 命中 %d 条, 期望 1（_ 应被当字面量）", total)
	}
}

// TestListHistory_StarredFilter 验证收藏筛选。
func TestListHistory_StarredFilter(t *testing.T) {
	s := newTestStore(t)
	_ = s.AddHistory(mkHistory("h1", "a", "b", true, ""), 0)
	_ = s.AddHistory(mkHistory("h2", "c", "d", false, ""), 0)
	_ = s.AddHistory(mkHistory("h3", "e", "f", true, ""), 0)

	yes := true
	no := false
	_, total, _ := s.ListHistory(HistoryQuery{Starred: &yes})
	if total != 2 {
		t.Errorf("只看收藏命中 %d 条, 期望 2", total)
	}
	_, total, _ = s.ListHistory(HistoryQuery{Starred: &no})
	if total != 1 {
		t.Errorf("只看未收藏命中 %d 条, 期望 1", total)
	}
	_, total, _ = s.ListHistory(HistoryQuery{Starred: nil})
	if total != 3 {
		t.Errorf("不限收藏命中 %d 条, 期望 3", total)
	}
}

// TestSetHistoryStarred 验证收藏切换与不存在 ID 的处理。
func TestSetHistoryStarred(t *testing.T) {
	s := newTestStore(t)
	_ = s.AddHistory(mkHistory("h1", "a", "b", false, ""), 0)
	if err := s.SetHistoryStarred("h1", true); err != nil {
		t.Fatalf("收藏失败: %v", err)
	}
	yes := true
	items, _, _ := s.ListHistory(HistoryQuery{Starred: &yes})
	if len(items) != 1 || items[0].ID != "h1" {
		t.Errorf("收藏未生效: %v", items)
	}
	// 取消收藏
	if err := s.SetHistoryStarred("h1", false); err != nil {
		t.Fatalf("取消收藏失败: %v", err)
	}
	_, total, _ := s.ListHistory(HistoryQuery{Starred: &yes})
	if total != 0 {
		t.Errorf("取消收藏后应无收藏项，得到 %d", total)
	}
	// 不存在的 ID
	if err := s.SetHistoryStarred("不存在", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("应返回 ErrNotFound，得到 %v", err)
	}
}

// TestDeleteHistory_Batch 验证批量删除与空输入。
func TestDeleteHistory_Batch(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 5; i++ {
		_ = s.AddHistory(mkHistory(fmt.Sprintf("h%d", i), "s", "t", false, ""), 0)
	}
	n, err := s.DeleteHistory([]string{"h0", "h2", "h4"})
	if err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if n != 3 {
		t.Errorf("删除 %d 条, 期望 3", n)
	}
	total, _ := s.CountHistory()
	if total != 2 {
		t.Errorf("剩余 %d 条, 期望 2", total)
	}
	// 空输入应返回 0 而非报错
	n, err = s.DeleteHistory(nil)
	if err != nil || n != 0 {
		t.Errorf("空输入应返回 (0,nil)，得到 (%d,%v)", n, err)
	}
	// 全空 ID 也应安全
	n, err = s.DeleteHistory([]string{"", "  "})
	if err != nil || n != 0 {
		t.Errorf("全空 ID 应返回 (0,nil)，得到 (%d,%v)", n, err)
	}
	// 部分不存在的 ID 只删存在的
	n, _ = s.DeleteHistory([]string{"h1", "不存在"})
	if n != 1 {
		t.Errorf("应只删 1 条，得到 %d", n)
	}
}

// TestClearHistory_KeepStarred 验证清空时保留收藏。
//
// 用户特意标星的内容通常有长期价值（例如常用术语的译法），
// 自动清理把收藏也删了会让收藏功能失去意义。
func TestClearHistory_KeepStarred(t *testing.T) {
	s := newTestStore(t)
	_ = s.AddHistory(mkHistory("h1", "a", "b", true, ""), 0)
	_ = s.AddHistory(mkHistory("h2", "c", "d", false, ""), 0)
	_ = s.AddHistory(mkHistory("h3", "e", "f", true, ""), 0)

	n, err := s.ClearHistory(true)
	if err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	if n != 1 {
		t.Errorf("应只删 1 条未收藏，得到 %d", n)
	}
	total, _ := s.CountHistory()
	if total != 2 {
		t.Errorf("剩余 %d 条, 期望 2（收藏应保留）", total)
	}
	// 不保留收藏时全清
	n, _ = s.ClearHistory(false)
	if n != 2 {
		t.Errorf("应删 2 条，得到 %d", n)
	}
	total, _ = s.CountHistory()
	if total != 0 {
		t.Errorf("应清空，剩余 %d", total)
	}
}

// TestPruneHistory_KeepsStarred 验证自动淘汰时保留收藏。
func TestPruneHistory_KeepsStarred(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 写 10 条，其中最早的两条是收藏
	for i := 0; i < 10; i++ {
		ts := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		starred := i < 2
		if err := s.AddHistory(mkHistory(fmt.Sprintf("h%02d", i), "s", "t", starred, ts), 5); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}
	total, _ := s.CountHistory()
	if total != 5 {
		t.Errorf("淘汰后剩 %d 条, 期望 5", total)
	}
	// 收藏的 h00/h01 必须还在
	yes := true
	starred, _, _ := s.ListHistory(HistoryQuery{Starred: &yes})
	if len(starred) != 2 {
		t.Errorf("收藏项剩 %d 条, 期望 2（自动淘汰不应删收藏）", len(starred))
	}
	ids := map[string]bool{}
	for _, it := range starred {
		ids[it.ID] = true
	}
	if !ids["h00"] || !ids["h01"] {
		t.Errorf("最早的收藏项被淘汰了: %v", ids)
	}
}

// TestPruneHistory_NoLimit 验证 limit<=0 时不淘汰。
func TestPruneHistory_NoLimit(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 20; i++ {
		if err := s.AddHistory(mkHistory(fmt.Sprintf("h%d", i), "s", "t", false, ""), 0); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}
	total, _ := s.CountHistory()
	if total != 20 {
		t.Errorf("limit=0 时不应淘汰，剩 %d 条, 期望 20", total)
	}
}

// TestPrefs_Defaults 验证首次读取返回完整默认值。
//
// 补齐而不是返回部分结果：前端拿到的一定是完整对象，
// 不需要在每个读取点都写 `prefs.x ?? default`。
func TestPrefs_Defaults(t *testing.T) {
	s := newTestStore(t)
	prefs, err := s.GetPrefs()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	// 所有默认键都应存在
	for k, want := range PrefDefaults {
		got, ok := prefs[k]
		if !ok {
			t.Errorf("缺少默认键 %s", k)
			continue
		}
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
			t.Errorf("默认值 %s = %v, 期望 %v", k, got, want)
		}
	}
	// 抽查几个 PLAN.md 规定的默认值
	if !numEq(prefs["instantDebounceMs"], 600) {
		t.Errorf("instantDebounceMs = %v, 期望 600", prefs["instantDebounceMs"])
	}
	if !numEq(prefs["historyLimit"], 500) {
		t.Errorf("historyLimit = %v, 期望 500", prefs["historyLimit"])
	}
	if !numEq(prefs["chunkDelayMs"], 1100) {
		t.Errorf("chunkDelayMs = %v, 期望 1100", prefs["chunkDelayMs"])
	}
	if prefs["defaultSourceLang"] != "auto" {
		t.Errorf("defaultSourceLang = %v, 期望 auto", prefs["defaultSourceLang"])
	}
}

// TestPrefs_RoundTrip 验证偏好写入与读回。
func TestPrefs_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	r0, _ := s.Rev()
	prefs, rev, err := s.SetPrefs(map[string]any{
		"instantEnabled":    false,
		"instantDebounceMs": 1200,
		"lastView":          "history",
		"defaultTargetLang": "en",
	}, r0)
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if rev != r0+1 {
		t.Errorf("rev = %d, 期望 %d", rev, r0+1)
	}
	if !boolEq(prefs["instantEnabled"], false) {
		t.Errorf("instantEnabled = %v, 期望 false", prefs["instantEnabled"])
	}
	if !numEq(prefs["instantDebounceMs"], 1200) {
		t.Errorf("instantDebounceMs = %v, 期望 1200", prefs["instantDebounceMs"])
	}
	if prefs["lastView"] != "history" {
		t.Errorf("lastView = %v, 期望 history", prefs["lastView"])
	}
	// 未写入的键应保持默认
	if !numEq(prefs["historyLimit"], 500) {
		t.Errorf("未写入的键应保持默认，historyLimit = %v", prefs["historyLimit"])
	}
	// 重新读取应一致
	again, _ := s.GetPrefs()
	if !numEq(again["instantDebounceMs"], 1200) {
		t.Errorf("重读不一致: %v", again["instantDebounceMs"])
	}
}

// TestPrefs_WhitelistRejectsUnknownKeys 验证未知键被忽略。
//
// 不限制键名的话，前端一个 bug 就可能把几 MB 垃圾塞进库里且永远清不掉。
func TestPrefs_WhitelistRejectsUnknownKeys(t *testing.T) {
	s := newTestStore(t)
	_, _, err := s.SetPrefs(map[string]any{
		"instantDebounceMs": 800,
		"未知键":               "垃圾值",
		"整个状态对象":            map[string]any{"a": 1},
	}, -1)
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	prefs, _ := s.GetPrefs()
	if _, ok := prefs["未知键"]; ok {
		t.Error("未知键不应被保存")
	}
	if _, ok := prefs["整个状态对象"]; ok {
		t.Error("未知键不应被保存")
	}
	if !numEq(prefs["instantDebounceMs"], 800) {
		t.Errorf("合法键应被保存，得到 %v", prefs["instantDebounceMs"])
	}
	// 库里应只有合法键
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM prefs`).Scan(&n); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if n != 1 {
		t.Errorf("prefs 表有 %d 行, 期望 1（只有合法键）", n)
	}
}

// TestPrefs_ClampsRanges 验证数值被夹到合法范围。
//
// 服务端必须自己守住边界：防抖 0ms 会让每次按键都发请求打爆配额，
// historyLimit 一百万会让库无限膨胀。不能信任前端传来的值。
func TestPrefs_ClampsRanges(t *testing.T) {
	s := newTestStore(t)
	cases := []struct {
		key  string
		in   any
		want int
	}{
		{"instantDebounceMs", 0, 200},        // 低于下限 200
		{"instantDebounceMs", 99999, 3000},   // 高于上限 3000
		{"instantDebounceMs", 600, 600},      // 范围内不变
		{"instantMinChars", 0, 1},            // 下限 1
		{"instantMinChars", 9999, 100},       // 上限 100
		{"instantMaxLength", 10, 100},        // 下限 100
		{"instantMaxLength", 999999, 100000}, // 上限 100000
		{"chunkDelayMs", -100, 0},            // 下限 0
		{"chunkDelayMs", 99999, 10000},       // 上限 10000
		{"maxCharsPerReq", 10, 500},          // 下限 500
		{"maxCharsPerReq", 9999999, 100000},  // 上限 100000
		{"historyLimit", 1, 100},             // 下限 100
		{"historyLimit", 999999, 5000},       // 上限 5000
	}
	for _, c := range cases {
		prefs, _, err := s.SetPrefs(map[string]any{c.key: c.in}, -1)
		if err != nil {
			t.Fatalf("%s 写入失败: %v", c.key, err)
		}
		got, ok := prefs[c.key].(int)
		if !ok {
			// JSON 往返后可能是 float64
			if f, ok2 := prefs[c.key].(float64); ok2 {
				got = int(f)
			} else {
				t.Errorf("%s 类型错误: %T", c.key, prefs[c.key])
				continue
			}
		}
		if got != c.want {
			t.Errorf("%s: 输入 %v 得到 %d, 期望 %d", c.key, c.in, got, c.want)
		}
	}
}

// TestPrefs_InvalidTypesFallBackToDefault 验证非法类型回退默认值。
func TestPrefs_InvalidTypesFallBackToDefault(t *testing.T) {
	s := newTestStore(t)
	prefs, _, err := s.SetPrefs(map[string]any{
		"instantDebounceMs": "不是数字",
		"instantEnabled":    "不是布尔",
		"lastView":          "不存在的视图",
	}, -1)
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	// 非法值应被丢弃，读回默认值
	again, _ := s.GetPrefs()
	if !numEq(again["instantDebounceMs"], 600) {
		t.Errorf("非法数值应回退默认 600，得到 %v", again["instantDebounceMs"])
	}
	if !boolEq(again["instantEnabled"], true) {
		t.Errorf("非法布尔应回退默认 true，得到 %v", again["instantEnabled"])
	}
	if again["lastView"] != "translate" {
		t.Errorf("非法视图名应回退默认 translate，得到 %v", again["lastView"])
	}
	_ = prefs
}

// TestPrefs_NaNAndInf 验证 NaN/Inf 被拦掉（否则比较结果不可预期）。
//
// 注意：NaN/Inf 无法写成 Go 常量表达式（编译期常量折叠会直接报
// "division by zero"），必须用 math 包在运行期构造。
// 它们也无法经 JSON 序列化，所以这里直接测 sanitizePrefs。
func TestPrefs_NaNAndInf(t *testing.T) {
	clean := sanitizePrefs(map[string]any{
		"instantDebounceMs": math.NaN(),
		"historyLimit":      math.Inf(1),
		"maxCharsPerReq":    math.Inf(-1),
	})
	if v, ok := clean["instantDebounceMs"].(int); !ok || v != 600 {
		t.Errorf("NaN 应回退默认 600，得到 %v", clean["instantDebounceMs"])
	}
	if v, ok := clean["historyLimit"].(int); !ok || v != 500 {
		t.Errorf("+Inf 应回退默认 500，得到 %v", clean["historyLimit"])
	}
	if v, ok := clean["maxCharsPerReq"].(int); !ok || v != 20000 {
		t.Errorf("-Inf 应回退默认 20000，得到 %v", clean["maxCharsPerReq"])
	}
}

// TestPrefs_KnownViews 验证 lastView 只接受已知视图名。
func TestPrefs_KnownViews(t *testing.T) {
	for _, v := range []string{"translate", "engines", "history", "settings"} {
		if !isKnownView(v) {
			t.Errorf("%s 应是合法视图名", v)
		}
	}
	for _, v := range []string{"", "unknown", "TRANSLATE", "../etc"} {
		if isKnownView(v) {
			t.Errorf("%q 不应是合法视图名", v)
		}
	}
}

// TestPrefs_Conflict 验证偏好写入也受乐观锁保护。
func TestPrefs_Conflict(t *testing.T) {
	s := newTestStore(t)
	r0, _ := s.Rev()
	// SetPrefs 返回 (prefs, newRev, error) —— 注意 rev 是第二个返回值
	_, r1, err := s.SetPrefs(map[string]any{"instantDebounceMs": 800}, r0)
	if err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	// 用旧 rev 再写 → 冲突
	_, _, err = s.SetPrefs(map[string]any{"instantDebounceMs": 900}, r0)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("旧 rev 应返回 ErrConflict，得到 %v", err)
	}
	// 数据未被覆盖
	prefs, _ := s.GetPrefs()
	if !numEq(prefs["instantDebounceMs"], 800) {
		t.Errorf("冲突时数据被覆盖: %v", prefs["instantDebounceMs"])
	}
	// 正确 rev 成功
	if _, _, err := s.SetPrefs(map[string]any{"instantDebounceMs": 900}, r1); err != nil {
		t.Errorf("正确 rev 应成功: %v", err)
	}
}

// TestPrefs_EmptyWriteDoesNotBumpRev 验证没有有效键时不递增 rev。
//
// 否则前端每次发一个空偏好对象都会让 rev 变化，
// 导致其他会话莫名其妙地收到冲突提示。
func TestPrefs_EmptyWriteDoesNotBumpRev(t *testing.T) {
	s := newTestStore(t)
	r0, _ := s.Rev()
	_, r1, err := s.SetPrefs(map[string]any{"未知键": "x"}, r0)
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if r1 != r0 {
		t.Errorf("无有效键时 rev 不应变化: %d -> %d", r0, r1)
	}
	_, r2, _ := s.SetPrefs(map[string]any{}, r0)
	if r2 != r0 {
		t.Errorf("空对象时 rev 不应变化: %d -> %d", r0, r2)
	}
}

// TestPrefs_CorruptedValueFallsBack 验证单条偏好损坏不影响整体加载。
func TestPrefs_CorruptedValueFallsBack(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.SetPrefs(map[string]any{"instantDebounceMs": 800}, -1); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	// 手工写入一条非法 JSON
	if _, err := s.db.Exec(`INSERT INTO prefs(key,value) VALUES('historyLimit','这不是JSON')
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
		t.Fatalf("准备失败: %v", err)
	}
	prefs, err := s.GetPrefs()
	if err != nil {
		t.Fatalf("单条损坏不应让整个加载失败: %v", err)
	}
	// 损坏的键应回退默认值
	if !numEq(prefs["historyLimit"], 500) {
		t.Errorf("损坏的键应回退默认 500，得到 %v", prefs["historyLimit"])
	}
	// 正常的键不受影响
	if !numEq(prefs["instantDebounceMs"], 800) {
		t.Errorf("正常键应保留，得到 %v", prefs["instantDebounceMs"])
	}
}

// TestNullStr 验证空串转 NULL 的辅助函数。
func TestNullStr(t *testing.T) {
	if nullStr("") != nil {
		t.Error("空串应转 NULL")
	}
	if nullStr("   ") != nil {
		t.Error("纯空白应转 NULL")
	}
	if nullStr("abc") != "abc" {
		t.Errorf("非空串应原样返回，得到 %v", nullStr("abc"))
	}
}

// TestEscapeLike 验证 LIKE 通配符转义。
func TestEscapeLike(t *testing.T) {
	cases := []struct{ in, want string }{
		{"abc", "abc"},
		{"100%", `100\%`},
		{"a_b", `a\_b`},
		{`a\b`, `a\\b`},
		{"", ""},
	}
	for _, c := range cases {
		if got := escapeLike(c.in); got != c.want {
			t.Errorf("escapeLike(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}
