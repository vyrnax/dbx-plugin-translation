package lang

import "testing"

// TestDetect 钉住文字体系判定。
//
// 重点是「门槛」：检测不到就宁可不判 —— 这个返回值会被拿去改变翻译方向，
// 误判比不判更糟（用户会看到自己没要求的语言对）。
func TestDetect(t *testing.T) {
	cases := []struct {
		text     string
		want     string
		wantBool bool
		why      string
	}{
		{"你好，今天天气怎么样？", "zh", true, "纯中文"},
		// 混排按「可归类字符多数」决定：这里 2 个汉字 vs 5 个拉丁字母 → 拉丁。
		// 不追求"见汉字就当中文"，那会让大量中英混排的技术文本被误换向。
		{"你好 world", "en", true, "中英混排按字符多数→拉丁"},
		{"获取用户 Info 信息", "zh", true, "汉字占多数时判为中文"},
		{"hello world", "en", true, "拉丁"},
		{"Bonjour, comment ça va ?", "en", true, "拉丁扩展字符仍算拉丁体系（代表码 en）"},
		{"今日はいい天気です", "ja", true, "假名→日语（假名优先级高于汉字，避免日文被认成中文）"},
		{"안녕하세요", "ko", true, "谚文→韩语"},
		{"Привет мир", "ru", true, "西里尔"},
		{"مرحبا بالعالم", "ar", true, "阿拉伯"},
		{"linearGradient", "en", true, "标识符是拉丁，不该被误判"},
		{"request_method", "en", true, "带下划线的标识符同上"},
		{"SELECT * FROM t WHERE id=1", "en", true, "SQL 里字母是拉丁"},
		{"12345", "", false, "纯数字不参与判定"},
		{"", "", false, "空串"},
		{"。", "", false, "只有一个标点"},
		{"a", "", false, "只有一个字母，样本不足"},
		{"", "", false, "空"},
	}
	for _, c := range cases {
		got, ok := Detect(c.text)
		if got != c.want || ok != c.wantBool {
			t.Errorf("Detect(%q) = (%q,%v)，期望 (%q,%v)｜%s", c.text, got, ok, c.want, c.wantBool, c.why)
		}
	}
}

// TestAutoDirection 是这次改动的核心：什么时候换、什么时候坚决不换。
func TestAutoDirection(t *testing.T) {
	type out struct{ src, dst, det string }
	cases := []struct {
		name        string
		src, dst    string
		text        string
		want        out
		changedWant bool
	}{
		{
			name: "目标是中文、原文是中文 → 换成译往英文",
			src:  "en", dst: "zh", text: "你好，今天天气怎么样？",
			want: out{Auto, "en", "zh"}, changedWant: true,
		},
		{
			name: "源语言是 auto 时同样换向（IDEA 的常见形态）",
			src:  Auto, dst: "zh", text: "你好",
			want: out{Auto, "en", "zh"}, changedWant: true,
		},
		{
			name: "目标英文、原文英文 → 换成译往中文",
			src:  Auto, dst: "en", text: "hello world",
			want: out{Auto, "zh", "en"}, changedWant: true,
		},
		{
			name: "拉丁原文 + 目标中文 → 不动（这就是用户要的翻译方向）",
			src:  Auto, dst: "zh", text: "hello world",
			want: out{Auto, "zh", "en"}, changedWant: false,
		},
		{
			name: "中文原文 + 目标英文 → 不动",
			src:  Auto, dst: "en", text: "你好",
			want: out{Auto, "en", "zh"}, changedWant: false,
		},
		{
			// 标识符是拉丁体系，与目标中文不冲突 → 一个字都不动。
			// 否则「翻译 request_method」会被改成译往英文，等于把分词的成果废掉。
			name: "拉丁标识符 + 目标中文 → 不换向",
			src:  "en", dst: "zh", text: "request_method",
			want: out{"en", "zh", "en"}, changedWant: false,
		},
		{
			name: "样本太小不判",
			src:  Auto, dst: "zh", text: "好",
			want: out{Auto, "zh", ""}, changedWant: false,
		},
	}
	for _, c := range cases {
		src, dst, det, changed := AutoDirection(c.src, c.dst, c.text)
		if changed != c.changedWant {
			t.Errorf("%s｜changed = %v，期望 %v（det=%q dst=%q）", c.name, changed, c.changedWant, det, dst)
			continue
		}
		if det != c.want.det {
			t.Errorf("%s｜detected = %q，期望 %q", c.name, det, c.want.det)
		}
		if !changed {
			// 没换向时必须原样返回，一个字段都不能动
			if src != c.src || dst != c.dst {
				t.Errorf("%s｜未换向却改动了语言对: %s/%s → %s/%s", c.name, c.src, c.dst, src, dst)
			}
			continue
		}
		if dst != c.want.dst || src != c.want.src {
			t.Errorf("%s｜换向结果 = %s/%s，期望 %s/%s", c.name, src, dst, c.want.src, c.want.dst)
		}
	}
}

// TestAutoDirection_NeverProducesSamePair 是护栏：
// 换向绝不能产出「源语言 == 目标语言」的荒唐组合（那必然让引擎原样吐回）。
func TestAutoDirection_NeverProducesSamePair(t *testing.T) {
	texts := []string{
		"你好，今天天气怎么样？", "hello world", "こんにちは、元気ですか",
		"안녕하세요 반갑습니다", "Привет, как дела", "linearGradient", "request_method",
	}
	pairs := [][2]string{{Auto, "zh"}, {"en", "zh"}, {"zh", "en"}, {Auto, "en"}, {"ja", "zh"}, {"ru", "en"}}
	for _, tx := range texts {
		for _, pr := range pairs {
			src, dst, _, _ := AutoDirection(pr[0], pr[1], tx)
			if dst == "auto" || dst == "" {
				t.Errorf("目标语言非法：text=%q %s/%s", tx, src, dst)
			}
			if src != Auto && src == dst {
				t.Errorf("换向后源目标同语言：text=%q %s/%s", tx, src, dst)
			}
		}
	}
}

// TestScript 复核语言码到体系的映射与默认回退。
func TestScript(t *testing.T) {
	m := map[string]string{
		"zh": ScriptHan, "zh-TW": ScriptHan, "ja": ScriptKana, "ko": ScriptHangul,
		"ru": ScriptCyrillic, "uk": ScriptCyrillic, "ar": ScriptArabic, "el": ScriptGreek,
		"en": ScriptLatin, "de": ScriptLatin, "vi": ScriptLatin, // 未列出的按拉丁
		"auto": "",
	}
	for code, want := range m {
		if got := Script(code); got != want {
			t.Errorf("Script(%q) = %q，期望 %q", code, got, want)
		}
	}
}
