// Package wordsplit 在把原文送给翻译引擎之前，把「标识符形态」的输入拆成空格短语。
//
// # 为什么需要它
//
// 数据库字段名与代码变量名几乎都是 camelCase 或 SCREAMING_SNAKE_CASE：
//
//	linearGradient  retrieveFishSurveyOrg  request_method  d_login_time  LOGIN_TIME
//
// 传统机器翻译引擎（百度 / 有道 / 阿里 / 腾讯 / DeepL）对这种「词典里查不到的整串」
// 通常【原样吐回】—— 译文等于原文，用户以为插件坏了。大模型引擎能自己看出词界，
// 效果尚可但并非每次都稳。
//
// 做法是【只在送引擎前插入空格】：界面上的原文不变、历史里存的也是原文，
// 引擎收到的是 "linear gradient" 这种真实短语，译文就正常了。
//
// # 为什么闸门收得这么紧、且几乎不改动字符
//
// 本包唯一允许的变换是「插入空格」。任何会改动字符本身的处理（压缩空白、去标点、
// 大小写归一）都可能改变语义 —— 大小写正是这些词的语义载体：
//
//	US → 美国      us → 我们
//	ID → 标识      it → 它
//	HE → 他        he → 他
//
// 因此规则是：
//  1. 只有整段就是一个标识符（或若干个以分隔符并列的标识符）时才处理；
//  2. 拆出的词里【全大写的一律原样保留】（HTTP / ID / XML），只把 camelCase 造成的
//     结构性大写降为小写（linearGradient 里那个 G 只是词界记号，不承载语义）；
//  3. 拆不出第二个词就什么都不做 —— 所以 "US" 与 "us" 都原样进引擎，
//     两者译文不同正是用户期待的结果。
//
// 词界规则参照已上架的 IDEA 插件 TranslationPlugin：
// util/CamelCaseSplitter.kt（状态机切分）与 util/Translations.kt 的 splitCamelCaseWords
// （整段命中 ^\w{2,}$ 才拆 + 全大写保留、其余小写）。
// 相对它有两处【有意的改进】，见 splitToken 注释。
package wordsplit

import (
	"strings"
	"unicode"
)

// maxWords 是一次预处理允许产出的最大词数。
// 超过它说明输入不像标识符（例如一大串以逗号分隔的散词），噪声大于收益，放弃处理。
const maxWords = 24

// 命名风格标识，仅用于界面提示「按什么规则拆的」。
const (
	StyleScreaming = "screaming" // USER_NAME
	StyleSnake     = "snake"     // user_name
	StylePascal    = "pascal"    // UserName
	StyleCamel     = "camel"     // userName
	StyleMixed     = "mixed"     // 标识符清单
)

// Result 是一次预处理的结论。
type Result struct {
	// Applied 为 false 时 Text 就是原文，其余附加字段无意义。
	Applied bool
	// Source 是用户输入的原文（永不改动；界面与历史都用它）。
	Source string
	// Text 是实际送给引擎的文本。
	Text string
	// Words 是拆出的词（已按「全大写保留」规则归一）。
	Words []string
	// Style 是识别出的命名风格。
	Style string
}

// Prepare 分析原文，给出「是否拆、拆成什么」。纯函数，无 IO。
func Prepare(text string) Result {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return notApplied(text)
	}

	// 情形一：整段就是一个标识符
	if isIdentifierToken(trimmed) {
		if words, ok := splitToken(trimmed); ok {
			return apply(text, trimmed, words)
		}
		return notApplied(text)
	}

	// 情形二：若干个标识符并列（贴 DB 字段清单的常见形态）
	if out, words, ok := splitList(trimmed); ok {
		return Result{Applied: true, Source: text, Text: out, Words: words, Style: StyleMixed}
	}

	return notApplied(text)
}

func notApplied(src string) Result {
	return Result{Source: src, Text: src}
}

func apply(src, token string, words []string) Result {
	joined := strings.Join(words, " ")
	if joined == token {
		// 拆完发现与原文一致（例如纯数字、全小写单词）—— 什么都不改
		return notApplied(src)
	}
	return Result{
		Applied: true,
		Source:  src,
		Text:    joined,
		Words:   words,
		Style:   detectStyle(token),
	}
}

// isIdentifierToken 判断整段是否「就是一个标识符」。
//
// 只接受 ASCII 字母、数字与下划线，且至少含 2 个字母：
//   - 中文/日文等自然语言文本、含空格或标点的句子、URL、代码片段都会被挡在门外
//   - 纯数字（"12345"）没有字母 → 拒绝
//
// 注意：这里不做任何大小写变换，"US" 会继续走到 splitToken，
// 但拆不出第二个词，最终原样返回。
func isIdentifierToken(s string) bool {
	letters := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '_', c >= '0' && c <= '9':
			// 允许出现，但不计入字母数
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			letters++
		default:
			return false
		}
	}
	return letters >= 2
}

// splitToken 把单个标识符切成词并归一化，返回词表。
//
// 词界规则：
//   - 小写/数字 → 大写 一定是词界：linearGradient → linear | Gradient
//   - 缩写串后面接小写时，把缩写串的最后一个大写划给下一个词：
//     XMLHttpRequest → XML | Http | Request（而不是 XMLH | Ttp | Request）
//
// 两处相对 Kotlin 版的有意改进：
//  1. 数字跟在字母后属于同一个词：utf8Encoder → utf8 | encoder
//     （Kotlin 版会切成 utf | 8 | encoder；DB 里 utf8 / md5 / sha256 这类后缀极多）
//  2. 分隔符直接丢弃且不产出空词：__init__ → init
func splitToken(s string) ([]string, bool) {
	rs := []rune(s)
	var words []string
	var cur []rune

	flush := func() {
		if len(cur) > 0 {
			words = append(words, string(cur))
			cur = cur[:0]
		}
	}
	// trailingUpper：当前词末尾连续大写的个数（用于识别缩写串）
	trailingUpper := func() int {
		n := 0
		for i := len(cur) - 1; i >= 0; i-- {
			if !unicode.IsUpper(cur[i]) {
				break
			}
			n++
		}
		return n
	}

	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if !isAlnum(c) { // 下划线、连字符等都是词界，本身丢弃
			flush()
			continue
		}
		if len(cur) > 0 {
			last := cur[len(cur)-1]
			switch {
			case unicode.IsUpper(c) && (unicode.IsLower(last) || unicode.IsDigit(last)):
				// linearGradient 的 G：新词开始
				flush()
			case unicode.IsLower(c) && unicode.IsUpper(last) && trailingUpper() >= 2:
				// XMLHttp 的 t：说明前面那串大写的最后一个其实属于新词的首字母，
				// 于是把最后一个大写挪出来，前面的部分先结算。
				moved := cur[len(cur)-1]
				cur = cur[:len(cur)-1]
				flush()
				cur = append(cur, moved)
			}
		}
		cur = append(cur, c)
	}
	flush()

	if len(words) < 2 {
		return nil, false
	}
	norm := make([]string, 0, len(words))
	for _, w := range words {
		if w = normalizeWord(w); w != "" {
			norm = append(norm, w)
		}
	}
	if len(norm) < 2 || len(norm) > maxWords {
		return nil, false
	}
	return norm, true
}

// normalizeWord 归一单词大小写 —— 整件事的要害所在。
//
// 全大写 → 原样保留：
//
//	USER_ID   → USER ID   （ID 不被降级，译文才会是「用户ID」而不是「用户身份证」）
//	HTTP_URL  → HTTP URL  （不变成 http url）
//
// 非全大写 → 小写：camelCase 里的大写只是词界记号，不承载语义，
// 小写更利于引擎识别（"linear gradient" 优于 "linear Gradient"）。
//
// 也就是说：只有「因为拆分而多出来的大写」被抹掉，原本的大写语义一律不动。
func normalizeWord(w string) string {
	if w == "" {
		return w
	}
	allUpper := true
	for _, r := range w {
		if !unicode.IsUpper(r) && !unicode.IsDigit(r) {
			allUpper = false
			break
		}
	}
	if allUpper {
		return w
	}
	return strings.ToLower(w)
}

// splitList 处理「多个标识符以任意分隔符并列」的输入。
//
// 条件苛刻是故意的：必须【每一个】非空片段都是标识符，且【至少一个】能拆出多词，
// 否则 "hello world" 这种正常短句也会被莫名改动。分隔符原样保留，
// 于是 "user_name, LOGIN_TIME" → "user name, LOGIN TIME"，逗号与顺序都不丢。
func splitList(s string) (string, []string, bool) {
	var (
		out     strings.Builder
		tok     strings.Builder
		all     []string
		tokens  int
		changed bool
		ok      = true
	)
	flushTok := func() {
		t := tok.String()
		tok.Reset()
		if t == "" {
			return
		}
		if !isIdentifierToken(t) {
			ok = false // 有片段不是标识符（普通单词、中文、带点号的名字…）
			return
		}
		tokens++
		if tokens > maxWords {
			ok = false
			return
		}
		if words, can := splitToken(t); can {
			out.WriteString(strings.Join(words, " "))
			all = append(all, words...)
			changed = true
		} else {
			out.WriteString(t)
			all = append(all, t)
		}
	}

	for _, r := range s {
		if isAlnum(r) || r == '_' {
			tok.WriteRune(r)
			continue
		}
		flushTok()
		if !ok {
			return "", nil, false
		}
		out.WriteRune(r) // 分隔符原样保留
	}
	flushTok()

	if !ok || tokens < 2 || !changed {
		return "", nil, false
	}
	return out.String(), all, true
}

// detectStyle 判断命名风格，只用于界面提示，不参与任何翻译决策。
func detectStyle(s string) string {
	hasUnder := strings.Contains(s, "_")
	allUpper := true
	first := rune(0)
	for _, r := range s {
		if unicode.IsLower(r) {
			allUpper = false
		}
		if unicode.IsLetter(r) && first == 0 {
			first = r
		}
	}
	switch {
	case hasUnder && allUpper:
		return StyleScreaming
	case hasUnder:
		return StyleSnake
	case unicode.IsUpper(first):
		return StylePascal
	default:
		return StyleCamel
	}
}

func isAlnum(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
