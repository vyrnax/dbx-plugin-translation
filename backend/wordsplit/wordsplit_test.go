package wordsplit

import "testing"

// TestPrepare_SplitCases 钉住「该拆的必须拆对」。
//
// 期望值全部来自本包注释里的规则（词界 + 全大写保留 + 数字附着），
// 不是「跑一遍看输出什么就写什么」—— 那是把 bug 固化成规范（见 AGENTS.md 6.8）。
func TestPrepare_SplitCases(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		style string
	}{
		// 用户提出的原始案例
		{"linearGradient", "linear gradient", StyleCamel},
		{"retrieveFishSurveyOrg", "retrieve fish survey org", StyleCamel},
		{"request_method", "request method", StyleSnake},
		{"d_login_time", "d login time", StyleSnake},
		{"LOGIN_TIME", "LOGIN TIME", StyleScreaming},

		// 缩写串：最后一个大写要划给下一个词
		// 只有全大写词被保留（XML）；Http / Request 里的大写是 camelCase 的词界记号，
		// 按规则降为小写 —— 与借鉴的 Kotlin 实现同一行为，不是漏改
		{"XMLHttpRequest", "XML http request", StylePascal},
		{"getHttpUrl", "get http url", StyleCamel},
		{"userID", "user ID", StyleCamel}, // ID 全大写 → 原样保留
		{"USER_ID", "USER ID", StyleScreaming},
		{"parseJSONBody", "parse JSON body", StyleCamel},

		// 数字附着（相对 Kotlin 版的改进）
		{"utf8Encoder", "utf8 encoder", StyleCamel},
		{"md5Hash", "md5 hash", StyleCamel},
		{"sha256Checksum", "sha256 checksum", StyleCamel},

		// 分隔符产生的空词要被丢掉
		{"__init__", "init", ""}, // 拆不出多词 → 不处理（见 NoopCases）
	}
	for _, c := range cases {
		got := Prepare(c.in)
		if c.style == "" {
			if got.Applied {
				t.Errorf("Prepare(%q) 不该处理，实际 Applied Text=%q", c.in, got.Text)
			}
			continue
		}
		if !got.Applied {
			t.Errorf("Prepare(%q) 应当拆分，实际未处理", c.in)
			continue
		}
		if got.Text != c.want {
			t.Errorf("Prepare(%q).Text = %q, 期望 %q", c.in, got.Text, c.want)
		}
		if got.Style != c.style {
			t.Errorf("Prepare(%q).Style = %q, 期望 %q", c.in, got.Style, c.style)
		}
		// 原文必须一字不动地留在 Source 里（界面与历史都显示它）
		if got.Source != c.in {
			t.Errorf("Prepare(%q).Source 被改动了: %q", c.in, got.Source)
		}
	}
}

// TestPrepare_NoopCases 钉住「不该动的绝对不能动」—— 这一组比拆对更重要。
//
// 尤其 US / us 这一对：它们的译文不同（美国 / 我们），
// 只要预处理抹掉大小写就会永远得到同一个结果，是用户明确点出的痛点。
func TestPrepare_NoopCases(t *testing.T) {
	noop := []string{
		"US", "us", "ID", "it", "HE", "he", // 大小写即语义，且拆不出多词
		"hello world",                // 正常短句，含空格
		"你好，世界",                      // 自然语言
		"12345",                      // 纯数字
		"a",                          // 单字符
		"https://a.com/x",            // URL
		"linear gradient",            // 已经是短语
		"SELECT * FROM t WHERE id=1", // SQL 片段
		"",                           // 空串
		"   ",                        // 只有空白
		"some very long prose with spaces and commas, yes", // 散文
	}
	for _, s := range noop {
		got := Prepare(s)
		if got.Applied {
			t.Errorf("Prepare(%q) 不该处理，实际 Text=%q", s, got.Text)
		}
		if got.Text != s {
			t.Errorf("Prepare(%q) 的送引擎文本被改动了: %q", s, got.Text)
		}
		if got.Source != s {
			t.Errorf("Prepare(%q) 的 Source 被改动了: %q", s, got.Source)
		}
	}
}

// TestPrepare_IdentifierList 覆盖 DB 场景常见的「字段清单」输入。
func TestPrepare_IdentifierList(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"user_name, LOGIN_TIME", "user name, LOGIN TIME"},
		{"id created_at updated_at", "id created at updated at"},
		{"request_method;", "request_method;"}, // 只有 1 个片段 + 尾部标点 → 不处理（规格边界）
		// 混入非标识符片段就必须整体放弃
		{"user_name, 你好", "user_name, 你好"},
		{"user_name, hello world", "user name, hello world"}, // 只有能拆的片段被改，散文式片段原样保留
	}
	for _, c := range cases {
		got := Prepare(c.in)
		wantApplied := got.Text != c.in
		if c.want == c.in {
			if wantApplied {
				t.Errorf("Prepare(%q) 不该处理，实际 Text=%q", c.in, got.Text)
			}
			continue
		}
		if !wantApplied {
			t.Errorf("Prepare(%q) 应当处理，实际未处理", c.in)
			continue
		}
		if got.Text != c.want {
			t.Errorf("Prepare(%q).Text = %q, 期望 %q", c.in, got.Text, c.want)
		}
	}
}

// TestPrepare_NeverChangesCase 是「大小写是语义」这条规则的显式护栏：
// 任何归一化都不允许把全大写词降级。
func TestPrepare_NeverChangesCase(t *testing.T) {
	got := Prepare("GET_HTTP_URL_TIMEOUT")
	if !got.Applied {
		t.Fatal("应当被处理")
	}
	want := "GET HTTP URL TIMEOUT"
	if got.Text != want {
		t.Errorf("Text = %q, 期望 %q（全大写词必须原样保留）", got.Text, want)
	}
}
