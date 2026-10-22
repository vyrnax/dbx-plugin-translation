package chunk

import (
	"strings"
	"testing"
)

// TestSplit_NoNeedWhenUnderLimit 验证不超限时原样返回，且不做任何切分。
// 这是最常见的路径（短文本翻译），必须零副作用。
func TestSplit_NoNeedWhenUnderLimit(t *testing.T) {
	in := "你好，世界。"
	r, err := Split(in, 100)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if len(r.Parts) != 1 || r.Parts[0] != in {
		t.Errorf("应原样返回单片段，得到 %v", r.Parts)
	}
	if r.HardCut {
		t.Error("未超限时 HardCut 应为 false")
	}
}

// TestSplit_Empty 验证空文本返回空 Parts 而非错误。
func TestSplit_Empty(t *testing.T) {
	r, err := Split("", 100)
	if err != nil {
		t.Fatalf("空文本不应报错: %v", err)
	}
	if len(r.Parts) != 0 {
		t.Errorf("空文本应返回空 Parts，得到 %v", r.Parts)
	}
}

// TestSplit_MaxCharsZero 验证 maxChars<=0 时视为不分段。
func TestSplit_MaxCharsZero(t *testing.T) {
	in := strings.Repeat("字", 500)
	for _, mc := range []int{0, -1} {
		r, err := Split(in, mc)
		if err != nil {
			t.Fatalf("maxChars=%d 不应报错: %v", mc, err)
		}
		if len(r.Parts) != 1 || r.Parts[0] != in {
			t.Errorf("maxChars=%d 应原样返回，得到 %d 段", mc, len(r.Parts))
		}
	}
}

// TestSplit_ParagraphBoundary 验证优先在段落边界切分。
func TestSplit_ParagraphBoundary(t *testing.T) {
	in := "第一段内容。\n\n第二段内容。\n\n第三段内容。"
	r, err := Split(in, 10)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if len(r.Parts) < 2 {
		t.Errorf("应切分为多段，得到 %d 段: %v", len(r.Parts), r.Parts)
	}
	for i, p := range r.Parts {
		if Count(p) > 10 {
			t.Errorf("第 %d 段超限: %d > 10 (%q)", i, Count(p), p)
		}
	}
	if r.HardCut {
		t.Error("有段落分隔符时不应硬切")
	}
}

// TestSplit_SentenceBoundary 验证无段落分隔时降级到句子边界。
func TestSplit_SentenceBoundary(t *testing.T) {
	in := "第一句话内容。第二句话内容。第三句话内容。第四句话内容。"
	r, err := Split(in, 12)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if len(r.Parts) < 2 {
		t.Errorf("应切分为多段，得到 %d 段", len(r.Parts))
	}
	for i, p := range r.Parts {
		if Count(p) > 12 {
			t.Errorf("第 %d 段超限: %d > 12 (%q)", i, Count(p), p)
		}
	}
	// 句号应被保留（拼接后能还原）
	if !strings.Contains(Join(r.Parts), "。") {
		t.Error("分隔符丢失")
	}
}

// TestSplit_RoundTrip 是最关键的不变量：拼接后必须完全还原原文。
// 如果分隔符被丢掉，译文结构就会改变（句号消失、段落合并），这是严重缺陷。
func TestSplit_RoundTrip(t *testing.T) {
	cases := []string{
		"第一段。\n\n第二段！\n\n第三段？",
		"句子一。句子二；句子三，句子四、句子五",
		"English sentence one. Sentence two! Sentence three?",
		"混合 mixed 文本 text with 分隔符 separators。And more!",
		strings.Repeat("长文本测试。", 50),
		"无分隔符的超长文本" + strings.Repeat("字", 200),
	}
	for _, in := range cases {
		r, err := Split(in, 20)
		if err != nil {
			t.Fatalf("切分失败: %v (输入长度 %d)", err, Count(in))
		}
		got := Join(r.Parts)
		if got != in {
			t.Errorf("拼接未还原原文:\n  原: %q\n  得: %q", in, got)
		}
		for i, p := range r.Parts {
			if Count(p) > 20 {
				t.Errorf("第 %d 段超限: %d > 20", i, Count(p))
			}
		}
	}
}

// TestSplit_MultibyteNeverBroken 验证多字节字符绝不会被切成两半。
// 按字节切分会产生非法 UTF-8，上游直接报错。
func TestSplit_MultibyteNeverBroken(t *testing.T) {
	in := strings.Repeat("中文字符测试", 30) // 每字 3 字节
	r, err := Split(in, 7)             // 刻意用非 3 倍数的上限
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	for i, p := range r.Parts {
		if !isValidUTF8(p) {
			t.Errorf("第 %d 段含非法 UTF-8（多字节字符被切断）: %q", i, p)
		}
		if Count(p) > 7 {
			t.Errorf("第 %d 段字符数超限: %d > 7", i, Count(p))
		}
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == 0xFFFD && !strings.Contains(s, "\uFFFD") {
			return false // 替换字符通常意味着解码失败
		}
	}
	// 更严格：逐字节校验
	for i := 0; i < len(s); {
		c := s[i]
		var size int
		switch {
		case c < 0x80:
			size = 1
		case c&0xE0 == 0xC0:
			size = 2
		case c&0xF0 == 0xE0:
			size = 3
		case c&0xF8 == 0xF0:
			size = 4
		default:
			return false
		}
		if i+size > len(s) {
			return false
		}
		for j := 1; j < size; j++ {
			if s[i+j]&0xC0 != 0x80 {
				return false
			}
		}
		i += size
	}
	return true
}

// TestSplit_ProtectsCodeBlock 验证代码块围栏内不被切分。
//
// maxChars 必须大于代码块本身长度，保护才有意义 —— 否则代码块超限会被硬切
// （见 TestSplit_OversizedProtectedHardCut）。
func TestSplit_ProtectsCodeBlock(t *testing.T) {
	code := "```go\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n```"
	in := "说明文字。\n" + code + "\n结尾文字。"
	r, err := Split(in, 60) // 大于代码块长度（约 40 字符）
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if !r.Protected {
		t.Error("含代码块时 Protected 应为 true")
	}
	if Join(r.Parts) != in {
		t.Errorf("拼接未还原原文:\n  原: %q\n  得: %q", in, Join(r.Parts))
	}
	// 代码块必须完整存在于某一段中
	found := false
	for _, p := range r.Parts {
		if strings.Contains(p, "```go") && strings.Contains(p, "fmt.Println") {
			found = true
		}
	}
	if !found {
		t.Errorf("代码块被切断了: %v", r.Parts)
	}
	if r.HardCut {
		t.Error("maxChars 足够时不应硬切")
	}
}

// TestSplit_OversizedProtectedHardCut 验证超长受保护单元被【硬切】而非整篇报错。
//
// 设计权衡：报错会让「文档里有一个长 URL」直接导致整篇翻译失败，
// 体验远差于「那一个 URL 被切断、其余正常翻译」。而硬切是必须的 ——
// 超过引擎上限的请求会被上游直接拒绝，宁可切断也不能发注定失败的请求。
func TestSplit_OversizedProtectedHardCut(t *testing.T) {
	longURL := "https://example.com/" + strings.Repeat("abcdefghij", 12) // 140 字符
	in := "看 " + longURL + " 这里。"
	r, err := Split(in, 30)
	if err != nil {
		t.Fatalf("超长 URL 应硬切而非报错，得到: %v", err)
	}
	if !r.HardCut {
		t.Error("超长受保护单元被切断时应置 HardCut=true，供 UI 提示质量下降")
	}
	if !r.Protected {
		t.Error("含 URL 时 Protected 应为 true")
	}
	for i, p := range r.Parts {
		if Count(p) > 30 {
			t.Errorf("第 %d 段超限: %d > 30", i, Count(p))
		}
	}
	if Join(r.Parts) != in {
		t.Error("硬切后拼接仍应还原原文")
	}
}

// TestSplit_ProtectsURL 验证 URL 不被切断（切断后链接失效）。
// maxChars 需大于 URL 长度，保护才生效。
func TestSplit_ProtectsURL(t *testing.T) {
	url := "https://api.example.com/v1/chat/completions"
	in := "请访问 " + url + " 获取详情，然后阅读文档。"
	r, err := Split(in, 60) // 大于 URL 长度（43 字符）
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	found := false
	for _, p := range r.Parts {
		if strings.Contains(p, url) {
			found = true
		}
	}
	if !found {
		t.Errorf("URL 被切断了: %v", r.Parts)
	}
	if Join(r.Parts) != in {
		t.Error("拼接未还原原文")
	}
}

// TestSplit_ProtectsPlaceholder 验证占位符不被切断（切断后无法回填）。
func TestSplit_ProtectsPlaceholder(t *testing.T) {
	cases := []string{
		"你好 {name}，欢迎使用 ${app} 应用。",
		"格式化 %s 和 %d 以及 %.2f 占位符。",
	}
	for _, in := range cases {
		r, err := Split(in, 12)
		if err != nil {
			t.Fatalf("不应报错: %v", err)
		}
		if Join(r.Parts) != in {
			t.Errorf("拼接未还原: %q", in)
		}
		// 占位符必须完整
		for _, ph := range []string{"{name}", "${app}", "%s", "%d", "%.2f"} {
			if !strings.Contains(in, ph) {
				continue
			}
			found := false
			for _, p := range r.Parts {
				if strings.Contains(p, ph) {
					found = true
				}
			}
			if !found {
				t.Errorf("占位符 %q 被切断了: %v", ph, r.Parts)
			}
		}
	}
}

// TestSplit_HardCutWhenNoSeparator 验证无分隔符的超长文本触发硬切并置标志。
func TestSplit_HardCutWhenNoSeparator(t *testing.T) {
	in := strings.Repeat("字", 100) // 无任何分隔符
	r, err := Split(in, 10)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if !r.HardCut {
		t.Error("无分隔符超长文本应置 HardCut=true，供 UI 提示质量下降")
	}
	if len(r.Parts) != 10 {
		t.Errorf("应硬切为 10 段，得到 %d 段", len(r.Parts))
	}
	if Join(r.Parts) != in {
		t.Error("硬切后拼接仍应还原原文")
	}
}

// TestSplit_VeryLongNeverErrors 验证再长的文本也不会返回 ErrTooLong。
//
// ErrTooLong 现在是防御性兜底：正常的超长文本（含超长 URL、超长代码块）
// 一律硬切，因为「整篇失败」比「局部被切断」体验差得多。
func TestSplit_VeryLongNeverErrors(t *testing.T) {
	cases := map[string]string{
		"超长URL": "看 https://example.com/" + strings.Repeat("abcdefghij", 60),
		"超长代码块": "```\n" + strings.Repeat("x", 800) + "\n```",
		"超长无分隔": strings.Repeat("字", 2000),
		"超长混合":  strings.Repeat("句子。{ph} ", 300),
	}
	for name, in := range cases {
		for _, lim := range []int{1, 5, 50} {
			r, err := Split(in, lim)
			if err != nil {
				t.Errorf("%s (limit=%d) 不应报错，得到: %v", name, lim, err)
				continue
			}
			if len(r.Parts) == 0 {
				t.Errorf("%s (limit=%d) 产出了 0 段", name, lim)
				continue
			}
			for i, p := range r.Parts {
				if Count(p) > lim {
					t.Errorf("%s (limit=%d) 第 %d 段超限: %d", name, lim, i, Count(p))
				}
			}
			if Join(r.Parts) != in {
				t.Errorf("%s (limit=%d) 拼接未还原原文", name, lim)
			}
		}
	}
}

// TestSplit_MaxCharsOne 验证极限上限 1 也能正常工作（每字符一段）。
func TestSplit_MaxCharsOne(t *testing.T) {
	in := "abc中文"
	r, err := Split(in, 1)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if len(r.Parts) != 5 {
		t.Errorf("应切为 5 段（每字符一段），得到 %d 段: %v", len(r.Parts), r.Parts)
	}
	if Join(r.Parts) != in {
		t.Error("拼接未还原原文")
	}
}

// TestSplit_ExactlyAtLimit 验证恰好等于上限时的边界行为。
func TestSplit_ExactlyAtLimit(t *testing.T) {
	in := strings.Repeat("字", 10)
	r, err := Split(in, 10)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if len(r.Parts) != 1 || r.Parts[0] != in {
		t.Errorf("恰好等于上限应原样返回，得到 %v", r.Parts)
	}
	if r.HardCut {
		t.Error("恰好等于上限不应硬切")
	}
}

// TestSplit_AllPartsWithinLimit 是通用不变量：任何情况下每段都不超限。
func TestSplit_AllPartsWithinLimit(t *testing.T) {
	inputs := []string{
		strings.Repeat("句子。", 100),
		strings.Repeat("word ", 100),
		strings.Repeat("段落\n\n", 50),
		"混合 mixed 123 文本。With punctuation! And more?",
		strings.Repeat("无分隔", 100),
	}
	limits := []int{5, 10, 50, 100}
	for _, in := range inputs {
		for _, lim := range limits {
			r, err := Split(in, lim)
			if err != nil {
				continue // ErrTooLong 是合法结果
			}
			for i, p := range r.Parts {
				if Count(p) > lim {
					t.Errorf("limit=%d 时第 %d 段超限: %d (%q)", lim, i, Count(p), truncate(p, 30))
				}
				if p == "" {
					t.Errorf("limit=%d 时第 %d 段为空串（会产生空翻译请求）", lim, i)
				}
			}
			if Join(r.Parts) != in {
				t.Errorf("limit=%d 时拼接未还原原文", lim)
			}
		}
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// TestCount_And_ByteSize 验证计数单位正确。
// 中文 UTF-8 占 3 字节：按字节计数会让 5000 字符上限实际只装下约 1666 个汉字。
func TestCount_And_ByteSize(t *testing.T) {
	s := "中文abc"
	if Count(s) != 5 {
		t.Errorf("Count = %d, 期望 5（按字符）", Count(s))
	}
	if ByteSize(s) != 9 {
		t.Errorf("ByteSize = %d, 期望 9（中3*2 + 英1*3）", ByteSize(s))
	}
	if Count("") != 0 || ByteSize("") != 0 {
		t.Error("空串计数应为 0")
	}
}

// TestSplitKeep_PreservesSeparators 验证分隔符保留在前一块末尾。
func TestSplitKeep_PreservesSeparators(t *testing.T) {
	pieces := splitKeep("一。二。三。", sentenceSep)
	if Join(pieces) != "一。二。三。" {
		t.Errorf("分隔符未保留: %v", pieces)
	}
	// 每块应以句号结尾（最后一块可能没有）
	for i := 0; i < len(pieces)-1; i++ {
		if !strings.HasSuffix(pieces[i], "。") {
			t.Errorf("第 %d 块未以分隔符结尾: %q", i, pieces[i])
		}
	}
}

// TestHardSplit_Multibyte 验证硬切按字符而非字节。
func TestHardSplit_Multibyte(t *testing.T) {
	in := strings.Repeat("中", 25)
	parts := hardSplit(in, 10)
	if len(parts) != 3 {
		t.Errorf("应切为 3 段（10+10+5），得到 %d 段", len(parts))
	}
	for i, p := range parts {
		if !isValidUTF8(p) {
			t.Errorf("第 %d 段非法 UTF-8", i)
		}
	}
	if Join(parts) != in {
		t.Error("硬切拼接应还原原文")
	}
}
