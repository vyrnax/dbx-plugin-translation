package youdao

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gaogao/translation/engine"
)

// TestTruncate_RuneBoundary 是有道签名正确性的【最关键】测试。
//
// 官方规则：若 len(q) <= 20 则原样；否则 q[:10] + len(q) + q[-10:]。
// 这里的 len 与切片都按【字符（rune）】计，不是字节。
//
// 用 len(string) 会得到字节数：中文一个字 3 字节，
// "你好世界" 的字节数是 12 而字符数是 4。按字节算的话，
// 一段 7 个汉字的文本（21 字节）会被误判为「超过 20」而触发截断，
// 且截断位置也会算错 —— 症状是「英文能翻、中文一直报 110/202 签名无效」，
// 极难排查，因为错误信息完全不会指向长度计算。
func TestTruncate_RuneBoundary(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// 短文本原样返回
		{"空串", "", ""},
		{"1字符", "a", "a"},
		{"恰好20个ASCII", strings.Repeat("a", 20), strings.Repeat("a", 20)},
		{"恰好20个汉字（60字节）", strings.Repeat("中", 20), strings.Repeat("中", 20)},

		// 21 个字符触发截断：前10 + "21" + 后10
		// 期望值直接写出字面结果，不用表达式拼 —— 拼接表达式本身可能算错，
		// 而测试的价值在于「期望值是独立推导出来的」。
		{"21个ASCII", strings.Repeat("a", 21), "aaaaaaaaaa21aaaaaaaaaa"},

		// 中文边界：21 个汉字 = 63 字节。若按字节算会误判为远超 20。
		{"21个汉字", strings.Repeat("中", 21),
			"中中中中中中中中中中21中中中中中中中中中中"},

		// 中英混排：21 个字符（10 ASCII + 4 汉字 + 7 数字）
		// 前 10 = "abcdefghij"，后 10 = "好世界1234567"（跳过第 11 个字符「你」）
		{"中英混排21字符", "abcdefghij你好世界1234567",
			"abcdefghij21好世界1234567"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := truncate(c.in)
			if got != c.want {
				t.Errorf("truncate(%q)\n  得: %q\n  期望: %q", c.in, got, c.want)
			}
		})
	}
}

// TestTruncate_MiddleIsOriginalLength 验证中间插入的是【原文总字符数】，
// 而不是截断后的长度，也不是字节数。
func TestTruncate_MiddleIsOriginalLength(t *testing.T) {
	in := strings.Repeat("中", 100) // 100 字符 = 300 字节
	got := truncate(in)
	want := strings.Repeat("中", 10) + "100" + strings.Repeat("中", 10)
	if got != want {
		t.Errorf("中间应是原文字符数 100\n  得: %q\n  期望: %q", got, want)
	}
	// 反向确认：不是字节数 300
	if strings.Contains(got, "300") {
		t.Errorf("中间误用了字节数 300: %q", got)
	}
}

// TestTruncate_ResultLength 验证截断结果长度合理（前10 + 数字 + 后10）。
func TestTruncate_ResultLength(t *testing.T) {
	in := strings.Repeat("字", 5000)
	got := truncate(in)
	r := []rune(got)
	// 10 + len("5000") + 10 = 24
	if len(r) != 24 {
		t.Errorf("截断结果长度 = %d, 期望 24（10+4+10）", len(r))
	}
	if !strings.HasPrefix(got, strings.Repeat("字", 10)) {
		t.Error("应以原文前 10 字符开头")
	}
	if !strings.HasSuffix(got, strings.Repeat("字", 10)) {
		t.Error("应以原文后 10 字符结尾")
	}
}

// newTestEngine 构造指向假端点的引擎。
func newTestEngine(t *testing.T, endpoint string) engine.Engine {
	t.Helper()
	e, err := engine.Build(Kind,
		engine.Config{"appKey": "testAppKey", "endpoint": endpoint},
		engine.Secrets{"appSecret": "testAppSecret"},
		engine.Options{Name: "测试有道"})
	if err != nil {
		t.Fatalf("构造引擎失败: %v", err)
	}
	return e
}

func req() engine.Request {
	return engine.Request{Source: "hello world", SourceLang: "en", TargetLang: "zh"}
}

// TestTranslate_SignatureIsCorrect 用 httptest 捕获请求，
// 按官方算法独立重算签名并比对。
//
// 这是签名正确性的可靠证明：不依赖外部服务，也不假设实现正确。
func TestTranslate_SignatureIsCorrect(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errorCode":"0","translation":["你好世界"],"l":"en2zh-CHS"}`))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL)
	res, err := e.Translate(context.Background(), req())
	if err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	if res.Target != "你好世界" {
		t.Errorf("译文 = %q", res.Target)
	}

	// 独立重算签名
	q := got.Get("q")
	salt := got.Get("salt")
	curtime := got.Get("curtime")
	if q != "hello world" {
		t.Errorf("q = %q", q)
	}
	if salt == "" || curtime == "" {
		t.Fatal("salt / curtime 不应为空")
	}
	signStr := "testAppKey" + truncate(q) + salt + curtime + "testAppSecret"
	sum := sha256.Sum256([]byte(signStr))
	want := hex.EncodeToString(sum[:])
	if got.Get("sign") != want {
		t.Errorf("签名不匹配\n  得: %s\n  期望: %s\n  signStr: %s",
			got.Get("sign"), want, signStr)
	}

	// 其余必填参数
	if got.Get("signType") != "v3" {
		t.Errorf("signType = %q, 期望 v3", got.Get("signType"))
	}
	if got.Get("appKey") != "testAppKey" {
		t.Errorf("appKey = %q", got.Get("appKey"))
	}
	if got.Get("from") != "en" {
		t.Errorf("from = %q, 期望 en", got.Get("from"))
	}
	if got.Get("to") != "zh-CHS" {
		t.Errorf("to = %q, 期望 zh-CHS（有道的简体中文码）", got.Get("to"))
	}
	// curtime 必须是秒级时间戳（10 位），不是毫秒（13 位）
	if len(curtime) != 10 {
		t.Errorf("curtime 长度 = %d, 期望 10（秒级时间戳）", len(curtime))
	}
}

// TestTranslate_RejectsAutoSource 验证有道不支持源语言自动检测。
//
// 有道是 6 类引擎里唯一不支持 auto 的。若不提前拦截，
// 上游会返回 108/110 这类看不懂的错误码。
func TestTranslate_RejectsAutoSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("不应发出请求：auto 源语言应在本地被拦截")
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL)
	_, err := e.Translate(context.Background(), engine.Request{
		Source: "hello", SourceLang: "auto", TargetLang: "zh",
	})
	if err == nil {
		t.Fatal("auto 源语言应被拒绝")
	}
	var ee *engine.Error
	if !asEngineErr(err, &ee) {
		t.Fatalf("应返回 *engine.Error，得到 %T", err)
	}
	if ee.Kind != engine.KindUnsupported {
		t.Errorf("应归类为 KindUnsupported，得到 %s", ee.KindName())
	}
	if !strings.Contains(ee.Message, "自动检测") {
		t.Errorf("错误消息应说明不支持自动检测: %s", ee.Message)
	}
}

// TestTranslate_MultiTranslationJoined 验证多段译文被拼接。
func TestTranslate_MultiTranslationJoined(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errorCode":"0","translation":["你好","世界"]}`))
	}))
	defer srv.Close()
	e := newTestEngine(t, srv.URL)
	res, err := e.Translate(context.Background(), req())
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if res.Target != "你好世界" {
		t.Errorf("多段译文应拼接，得到 %q", res.Target)
	}
}

// TestTranslate_ErrorCodes 验证业务错误码映射。
func TestTranslate_ErrorCodes(t *testing.T) {
	cases := []struct {
		code string
		kind engine.ErrorKind
	}{
		{"108", engine.KindAuth},        // 应用 ID 无效
		{"202", engine.KindAuth},        // 签名校验失败
		{"110", engine.KindConfig},      // 无相关服务（未开通）
		{"203", engine.KindConfig},      // IP 不在白名单
		{"206", engine.KindConfig},      // 时间戳错误
		{"109", engine.KindRateLimit},   // 批量频次受限
		{"411", engine.KindRateLimit},   // 访问频率受限
		{"401", engine.KindRateLimit},   // 账户欠费
		{"102", engine.KindUnsupported}, // 不支持的语言
		{"103", engine.KindConfig},      // 文本过长
		{"999", engine.KindUpstream},    // 未知码保守归为上游错误
	}
	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"errorCode": c.code})
			}))
			defer srv.Close()
			e := newTestEngine(t, srv.URL)
			_, err := e.Translate(context.Background(), req())
			if err == nil {
				t.Fatalf("errorCode=%s 应报错", c.code)
			}
			var ee *engine.Error
			if !asEngineErr(err, &ee) {
				t.Fatalf("应返回 *engine.Error，得到 %T", err)
			}
			if ee.Kind != c.kind {
				t.Errorf("errorCode=%s 应归类为 %s，得到 %s（%s）",
					c.code, kindName(c.kind), ee.KindName(), ee.Message)
			}
			if !strings.Contains(ee.Message, c.code) {
				t.Errorf("错误消息应包含原始错误码 %s 便于排查: %s", c.code, ee.Message)
			}
		})
	}
}

// TestBuild_Validation 验证构造期校验。
func TestBuild_Validation(t *testing.T) {
	if _, err := engine.Build(Kind, engine.Config{}, engine.Secrets{"appSecret": "s"}, engine.Options{}); err == nil {
		t.Error("缺 appKey 应报错")
	}
	if _, err := engine.Build(Kind, engine.Config{"appKey": "k"}, engine.Secrets{}, engine.Options{}); err == nil {
		t.Error("缺 appSecret 应报错")
	}
	// 默认端点
	e, err := engine.Build(Kind, engine.Config{"appKey": "k"}, engine.Secrets{"appSecret": "s"}, engine.Options{})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if e.Kind() != Kind || e.SupportsStreaming() || e.MaxChars() <= 0 {
		t.Errorf("接口契约不符: kind=%s streaming=%v maxChars=%d",
			e.Kind(), e.SupportsStreaming(), e.MaxChars())
	}
}

// TestTranslate_TooLong 验证超长文本在本地被拦截（不发请求）。
func TestTranslate_TooLong(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("超长文本不应发出请求")
	}))
	defer srv.Close()
	e := newTestEngine(t, srv.URL)
	_, err := e.Translate(context.Background(), engine.Request{
		Source: strings.Repeat("字", maxCharsPerRequest+1), SourceLang: "en", TargetLang: "zh",
	})
	if err == nil {
		t.Fatal("超长文本应被拒绝")
	}
	var ee *engine.Error
	if !asEngineErr(err, &ee) || ee.Kind != engine.KindConfig {
		t.Errorf("应归类为 KindConfig，得到 %v", err)
	}
}

// asEngineErr 是 errors.As 的包装（避免每个测试文件都导入 errors）。
func asEngineErr(err error, target **engine.Error) bool {
	for err != nil {
		if e, ok := err.(*engine.Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func kindName(k engine.ErrorKind) string {
	e := &engine.Error{Kind: k}
	return e.KindName()
}
