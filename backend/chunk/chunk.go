// Package chunk 把长文本切分成不超过引擎单次上限的片段。
//
// 为什么需要分段：
//
//	各引擎的单次请求上限差异极大 —— 百度 6000 字节、有道 5000 字符、
//	阿里 5000 字符、DeepL 128 KiB。用户粘贴一篇长文时，不分段就会被上游直接拒绝，
//	而错误信息往往是「参数非法」这类看不懂的提示。
//
// 分段的核心难点不是「切开」，而是【切在哪里】：
//
//	按固定长度硬切会把句子拦腰截断，导致译文出现半句话；
//	更糟的是会切断代码块、URL 和 {placeholder} 占位符 —— 这些一旦断开，
//	译文里的代码就跑不起来了，占位符也无法回填。
//
// 因此本包按「段落 → 句子 → 子句 → 硬切」的优先级逐级降级，
// 并在切分前先把不可分割单元（代码块围栏、URL、占位符）保护起来。
//
// 计数单位一律是【rune（字符）】而非字节：
//
//	中文 UTF-8 占 3 字节，按字节计数会让「5000 字符」的上限实际只装下
//	约 1666 个汉字，白白浪费配额；更严重的是按字节切分会把多字节字符
//	切成两半，产生非法 UTF-8，上游直接报错。
package chunk

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// ErrTooLong 表示文本无法在给定上限内切分。
//
// 调用方应把它映射为错误码 -32036，并提示用户缩短文本或换用上限更高的引擎。
//
// 触发条件很窄：正常的超长文本（含超长 URL、超长代码块）都会被硬切而不是报错，
// 因为「整篇失败」比「局部被切断」的体验差得多。本错误只在切分逻辑无法产出
// 任何有效片段时作为防御性兜底返回（例如 maxChars 为正但文本结构异常）。
var ErrTooLong = errors.New("文本包含无法切分的超长片段")

// Result 是分段结果。
type Result struct {
	// Parts 是按原文顺序排列的片段。拼接后应能还原原文（含分隔符）。
	Parts []string
	// HardCut 为 true 表示发生过「硬切」——即某个句子/子句本身超过上限，
	// 被按字符数强行截断。此时译文质量会下降，UI 应给出提示。
	HardCut bool
	// Protected 为 true 表示原文含受保护单元（代码块/URL/占位符）。
	Protected bool
}

// 分隔符集合，按优先级从高到低。
//
// 段落分隔优先于句子分隔：段落是语义上最自然的边界，切开它几乎不影响理解。
// 句子分隔次之。子句分隔（逗号/分号）是最后的「软」手段，
// 切在这里译文会略显生硬，但总比硬切好。
var (
	paragraphSep = []string{"\n\n", "\r\n\r\n"}
	sentenceSep  = []string{"。", "！", "？", "；", ".", "!", "?", ";", "\n", "\r"}
	clauseSep    = []string{"，", "、", ",", " ", "\t"}
)

// placeholderMarkers 是「不可分割单元」的识别标记。
//
// 这些片段一旦断开就失去意义：
//   - ``` 围栏内的代码块：断开后代码语法错误
//   - http(s):// URL：断开后链接失效
//   - {xxx} / ${xxx} / %s / %d 占位符：断开后无法回填
//   - <tag> HTML 标签：断开后标签不闭合
const (
	maxPlaceholderLen = 512 // 单个受保护单元的长度上限，超过则不保护（避免整篇被当成一个单元）
)

// Split 把 text 切分为每段不超过 maxChars 个【字符】的片段。
//
// maxChars <= 0 时视为不分段，直接返回单片段（调用方已确认不超限）。
// 空文本返回空 Parts 而非错误 —— 「翻译空字符串」是合法输入，
// 应由上层决定是拒绝还是直接返回空译文。
//
// 返回 ErrTooLong 的唯一情况：某个不可分割单元本身就超过 maxChars，
// 且它无法被进一步切分（例如一个 8000 字符的超长 URL）。
func Split(text string, maxChars int) (Result, error) {
	if text == "" {
		return Result{Parts: []string{}}, nil
	}
	if maxChars <= 0 {
		return Result{Parts: []string{text}}, nil
	}

	// 整体不超限：直接返回，不做任何切分。
	// 这是最常见的路径（短文本翻译），必须最快返回。
	if utf8.RuneCountInString(text) <= maxChars {
		return Result{Parts: []string{text}, Protected: hasProtected(text)}, nil
	}

	// 先按受保护单元把文本拆成「可切」与「不可切」交替的序列。
	segs := segment(text)

	var parts []string
	hardCut := false
	protected := false
	var buf strings.Builder // 当前正在累积的片段

	flush := func() {
		if buf.Len() > 0 {
			parts = append(parts, buf.String())
			buf.Reset()
		}
	}

	for _, s := range segs {
		if s.protected {
			protected = true
			n := utf8.RuneCountInString(s.text)
			if n > maxChars {
				// 受保护单元自身超过引擎上限。
				//
				// 这里选择【硬切】而不是报错，是权衡后的决定：
				//
				//	报错会让「文档里有一个长 URL / 长代码块」直接导致整篇翻译失败，
				//	用户体验远差于「那一个 URL 被切断、其余部分正常翻译」。
				//	而硬切是必须的 —— 超过引擎上限的请求会被上游直接拒绝，
				//	宁可切断也不能发一个注定失败的请求。
				//
				// 置 HardCut=true 让 UI 提示「部分内容可能被切断，译文质量下降」。
				hardCut = true
				flush()
				for _, hc := range hardSplit(s.text, maxChars) {
					parts = append(parts, hc)
				}
				continue
			}
			// 放进当前 buf 会超限 → 先 flush，再单独成段
			if utf8.RuneCountInString(buf.String())+n > maxChars {
				flush()
			}
			buf.WriteString(s.text)
			if utf8.RuneCountInString(buf.String()) >= maxChars {
				flush()
			}
			continue
		}

		// 普通文本：按分隔符优先级切分
		pieces := splitBySeparators(s.text, maxChars, &hardCut)
		for _, p := range pieces {
			pn := utf8.RuneCountInString(p)
			if pn > maxChars {
				// splitBySeparators 已尽力，仍超限说明是硬切产物
				hardCut = true
				for _, hc := range hardSplit(p, maxChars) {
					if utf8.RuneCountInString(buf.String())+utf8.RuneCountInString(hc) > maxChars {
						flush()
					}
					buf.WriteString(hc)
					if utf8.RuneCountInString(buf.String()) >= maxChars {
						flush()
					}
				}
				continue
			}
			if utf8.RuneCountInString(buf.String())+pn > maxChars {
				flush()
			}
			buf.WriteString(p)
			if utf8.RuneCountInString(buf.String()) >= maxChars {
				flush()
			}
		}
	}
	flush()

	if len(parts) == 0 {
		return Result{}, ErrTooLong
	}
	return Result{Parts: parts, HardCut: hardCut, Protected: protected}, nil
}

// segment 把文本拆成「受保护」与「普通」交替的片段序列。
type seg struct {
	text      string
	protected bool
}

func segment(text string) []seg {
	var out []seg
	i := 0
	n := len(text)
	start := 0

	flushPlain := func(end int) {
		if end > start {
			out = append(out, seg{text: text[start:end], protected: false})
		}
	}

	for i < n {
		// 1) 代码块围栏 ```
		if strings.HasPrefix(text[i:], "```") {
			// 找到闭合围栏（从 i+3 开始找下一个 ```）
			end := strings.Index(text[i+3:], "```")
			var stop int
			if end < 0 {
				stop = n // 未闭合：保护到文末
			} else {
				stop = i + 3 + end + 3
			}
			if stop-i <= maxPlaceholderLen {
				flushPlain(i)
				out = append(out, seg{text: text[i:stop], protected: true})
				i = stop
				start = i
				continue
			}
		}
		// 2) 行内代码 `...`
		if text[i] == '`' {
			if end := strings.IndexByte(text[i+1:], '`'); end >= 0 {
				stop := i + 1 + end + 1
				if stop-i <= maxPlaceholderLen {
					flushPlain(i)
					out = append(out, seg{text: text[i:stop], protected: true})
					i = stop
					start = i
					continue
				}
			}
		}
		// 3) URL
		if strings.HasPrefix(text[i:], "http://") || strings.HasPrefix(text[i:], "https://") {
			stop := i
			for stop < n && !isURLBreak(text[stop]) {
				stop++
			}
			if stop-i <= maxPlaceholderLen {
				flushPlain(i)
				out = append(out, seg{text: text[i:stop], protected: true})
				i = stop
				start = i
				continue
			}
		}
		// 4) ${xxx} 与 {xxx} 占位符
		if text[i] == '{' || (text[i] == '$' && i+1 < n && text[i+1] == '{') {
			open := i
			if text[i] == '$' {
				i++
			}
			if stop := strings.IndexByte(text[i+1:], '}'); stop >= 0 {
				endPos := i + 1 + stop + 1
				if endPos-open <= maxPlaceholderLen {
					flushPlain(open)
					out = append(out, seg{text: text[open:endPos], protected: true})
					i = endPos
					start = i
					continue
				}
			}
			i = open
		}
		// 5) printf 风格占位符 %s %d %.2f 等
		if text[i] == '%' && i+1 < n {
			j := i + 1
			for j < n && (text[j] == '-' || text[j] == '+' || text[j] == '#' ||
				text[j] == ' ' || (text[j] >= '0' && text[j] <= '9') || text[j] == '.') {
				j++
			}
			if j < n && strings.IndexByte("sdvfgqetxXocbU", text[j]) >= 0 {
				flushPlain(i)
				out = append(out, seg{text: text[i : j+1], protected: true})
				i = j + 1
				start = i
				continue
			}
		}
		// 6) HTML 标签 <tag ...>
		if text[i] == '<' {
			if stop := strings.IndexByte(text[i:], '>'); stop >= 0 && stop <= 128 {
				flushPlain(i)
				out = append(out, seg{text: text[i : i+stop+1], protected: true})
				i = i + stop + 1
				start = i
				continue
			}
		}
		i++
	}
	flushPlain(n)
	return out
}

// isURLBreak 判断字符是否应终止 URL 扫描。
// 空白与常见句末标点会终止 URL；但标点若是 URL 的一部分（如 query 里的 & =）则不终止。
func isURLBreak(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '"', '\'', '<', '>', '`':
		return true
	}
	return false
}

// hasProtected 快速判断文本是否含受保护单元（用于整体不超限时的 Result.Protected）。
func hasProtected(text string) bool {
	return strings.Contains(text, "```") ||
		strings.Contains(text, "http://") ||
		strings.Contains(text, "https://") ||
		strings.Contains(text, "{") ||
		strings.Contains(text, "<")
}

// splitBySeparators 按分隔符优先级把普通文本切成不超过 maxChars 的小块。
//
// 逐级降级：先试段落分隔，切出来的块若仍超限再用句子分隔，再超限用子句分隔。
// hardCut 指针用于回传「是否发生过硬切」。
func splitBySeparators(text string, maxChars int, hardCut *bool) []string {
	if utf8.RuneCountInString(text) <= maxChars {
		return []string{text}
	}
	pieces := splitKeep(text, paragraphSep)
	if len(pieces) == 1 {
		pieces = splitKeep(text, sentenceSep)
	}
	// 对仍超限的块继续用子句分隔降级
	var out []string
	for _, p := range pieces {
		if utf8.RuneCountInString(p) <= maxChars {
			out = append(out, p)
			continue
		}
		sub := splitKeep(p, clauseSep)
		for _, s := range sub {
			if utf8.RuneCountInString(s) <= maxChars {
				out = append(out, s)
				continue
			}
			// 子句仍超限 → 只能硬切
			*hardCut = true
			out = append(out, s) // 交给上层 hardSplit 处理
		}
	}
	return out
}

// splitKeep 按分隔符切分，并把分隔符【保留在前一块的末尾】。
//
// 保留分隔符是关键：拼接后必须能还原原文。如果丢掉分隔符，
// 译文拼起来会变成「你好世界」而不是「你好。世界。」，句号全没了。
func splitKeep(text string, seps []string) []string {
	var out []string
	var buf strings.Builder
	runes := []rune(text)

	for i := 0; i < len(runes); {
		matched := ""
		for _, s := range seps {
			sr := []rune(s)
			if i+len(sr) <= len(runes) && string(runes[i:i+len(sr)]) == s {
				matched = s
				break
			}
		}
		buf.WriteRune(runes[i])
		i++
		if matched != "" {
			// 多字符分隔符（如 \n\n）的剩余部分也要写入
			for k := 1; k < len([]rune(matched)); k++ {
				if i < len(runes) {
					buf.WriteRune(runes[i])
					i++
				}
			}
			out = append(out, buf.String())
			buf.Reset()
		}
	}
	if buf.Len() > 0 {
		out = append(out, buf.String())
	}
	if len(out) == 0 {
		return []string{text}
	}
	return out
}

// hardSplit 按 maxChars 个【字符】强行切分（不按字节，避免切断多字节字符）。
//
// 这是最后手段，只在句子/子句本身超限时使用。切出来的译文质量会下降，
// 因此 Result.HardCut 会被置为 true，UI 据此提示用户。
func hardSplit(text string, maxChars int) []string {
	runes := []rune(text)
	if len(runes) <= maxChars {
		return []string{text}
	}
	var out []string
	for i := 0; i < len(runes); i += maxChars {
		end := i + maxChars
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[i:end]))
	}
	return out
}

// Join 把分段翻译结果按顺序拼接。
//
// 因为 Split 保留了分隔符，直接拼接即可还原结构，不需要额外插入空格或换行。
// 这也是为什么 splitKeep 必须把分隔符留在前一块末尾 —— 否则这里就得猜
// 该补什么分隔符，而猜错就会改变原文结构。
func Join(parts []string) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p)
	}
	return b.String()
}

// Count 返回文本的字符数（rune 数），供上层做上限预检。
func Count(text string) int {
	return utf8.RuneCountInString(text)
}

// ByteSize 返回文本的 UTF-8 字节数。
//
// 百度翻译的上限是按【字节】计的（6000 字节），与其他引擎按字符计不同，
// 因此需要单独提供字节计数，由 baidu 适配器自行换算。
func ByteSize(text string) int {
	return len(text)
}
