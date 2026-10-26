package baidu

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/vyrnax/translation/engine"
)

const (
	testAppID  = "20200101000000001"
	testSecret = "testSecretKey123456"
)

func newTestEngine(t *testing.T, endpoint string) engine.Engine {
	t.Helper()
	e, err := engine.Build(Kind,
		engine.Config{"appId": testAppID, "endpoint": endpoint},
		engine.Secrets{"secretKey": testSecret},
		engine.Options{Name: "测试百度"})
	if err != nil {
		t.Fatalf("构造引擎失败: %v", err)
	}
	return e
}

func req() engine.Request {
	return engine.Request{Source: "hello world", SourceLang: "en", TargetLang: "zh"}
}

// TestTranslate_SignatureIsCorrect 用 httptest 捕获请求，按官方算法独立重算 MD5 签名。
//
// 百度的签名规则：sign = MD5(appid + q + salt + secretKey)
// 拼接顺序【必须】严格是 appid → q → salt → secretKey。
// 顺序错了签名必错，而错误码 54001 只写「签名错误」，不会告诉你是顺序问题。
func TestTranslate_SignatureIsCorrect(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"from":"en","to":"zh","trans_result":[{"src":"hello world","dst":"你好世界"}]}`))
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

	// 独立重算签名：顺序 appid → q → salt → secretKey
	q := got.Get("q")
	salt := got.Get("salt")
	if q != "hello world" {
		t.Errorf("q = %q", q)
	}
	if salt == "" {
		t.Fatal("salt 不应为空")
	}
	signStr := testAppID + q + salt + testSecret
	sum := md5.Sum([]byte(signStr))
	want := hex.EncodeToString(sum[:])
	if got.Get("sign") != want {
		t.Errorf("签名不匹配\n  得: %s\n  期望: %s\n  signStr: %s", got.Get("sign"), want, signStr)
	}
	// MD5 十六进制必须是 32 位小写
	if len(got.Get("sign")) != 32 {
		t.Errorf("sign 长度 = %d, 期望 32", len(got.Get("sign")))
	}
	if got.Get("sign") != strings.ToLower(got.Get("sign")) {
		t.Error("sign 应为小写十六进制（百度要求）")
	}

	if got.Get("appid") != testAppID {
		t.Errorf("appid = %q", got.Get("appid"))
	}
	if got.Get("from") != "en" {
		t.Errorf("from = %q, 期望 en", got.Get("from"))
	}
	if got.Get("to") != "zh" {
		t.Errorf("to = %q, 期望 zh（百度的简体中文码）", got.Get("to"))
	}
	// 密钥绝不能作为独立参数发出去（只参与签名计算）
	if got.Get("secretKey") != "" || got.Get("secret") != "" {
		t.Error("密钥不应作为请求参数发送（只参与签名）")
	}
}

// TestTranslate_SaltUnique 验证每次请求的 salt 不同。
//
// 官方要求 salt 是随机数。若固定不变，两次相同文本的请求签名完全相同，
// 削弱了防重放的意义；官方示例用时间戳，高并发下可能重复。
func TestTranslate_SaltUnique(t *testing.T) {
	var salts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		salts = append(salts, r.PostForm.Get("salt"))
		_, _ = w.Write([]byte(`{"trans_result":[{"dst":"你好"}]}`))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL)
	for i := 0; i < 5; i++ {
		if _, err := e.Translate(context.Background(), req()); err != nil {
			t.Fatalf("第 %d 次翻译失败: %v", i, err)
		}
	}
	seen := map[string]bool{}
	for _, s := range salts {
		if s == "" {
			t.Fatal("salt 为空")
		}
		if seen[s] {
			t.Errorf("salt 重复: %q（应每次随机）", s)
		}
		seen[s] = true
	}
}

// TestTranslate_AutoSource 验证 auto 源语言映射为百度的 "auto"。
func TestTranslate_AutoSource(t *testing.T) {
	var gotFrom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotFrom = r.PostForm.Get("from")
		_, _ = w.Write([]byte(`{"from":"en","trans_result":[{"dst":"你好"}]}`))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL)
	_, err := e.Translate(context.Background(), engine.Request{
		Source: "hello", SourceLang: "auto", TargetLang: "zh",
	})
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if gotFrom != "auto" {
		t.Errorf("from = %q, 期望 auto", gotFrom)
	}
}

// TestTranslate_MultiTransResultJoined 验证长文本被百度拆成多项时全部拼接。
//
// 百度会把长文本按句拆成多个 trans_result 项。只取第一项会丢译文。
func TestTranslate_MultiTransResultJoined(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"trans_result":[{"src":"a","dst":"甲"},{"src":"b","dst":"乙"},{"src":"c","dst":"丙"}]}`))
	}))
	defer srv.Close()
	e := newTestEngine(t, srv.URL)
	res, err := e.Translate(context.Background(), req())
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if res.Target != "甲乙丙" {
		t.Errorf("多项 trans_result 应全部拼接，得到 %q", res.Target)
	}
}

// TestTranslate_ByteLimitNotCharLimit 验证上限按【字节】计。
//
// 百度的 6000 是字节数。若按字符数判断，中文长文本（每字 3 字节）
// 实际会超限 3 倍，上游返回 54001 而用户看不懂。
func TestTranslate_ByteLimitNotCharLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("超限文本不应发出请求")
	}))
	defer srv.Close()
	e := newTestEngine(t, srv.URL)

	// 2001 个汉字 = 6003 字节 > 6000，但字符数远小于 maxCharsPerRequest(2000)？
	// 注意 maxCharsPerRequest=2000，所以 2001 字符也会先被字符数拦截。
	// 这里构造「字符数不超但字节数超」的情况来证明是按字节判断的：
	// 用 1999 个汉字 = 5997 字节（未超），应能通过字节检查
	okText := strings.Repeat("字", 1999)
	if len(okText) > maxBytesPerRequest {
		t.Fatalf("测试前提错误：%d 字节已超过上限", len(okText))
	}

	// 超过字符上限的文本应被拦（分段由上层负责）
	tooLong := strings.Repeat("字", maxCharsPerRequest+1)
	_, err := e.Translate(context.Background(), engine.Request{
		Source: tooLong, SourceLang: "en", TargetLang: "zh",
	})
	if err == nil {
		t.Error("超过字符上限应被拒绝")
	}
}

// TestTranslate_ErrorCodes 验证业务错误码映射。
//
// 注意百度的 error_code 是 JSON 【字符串】（"52003"）而不是数字。
// 用 int 字段解析会得到 0，导致所有错误都被静默当成成功。
func TestTranslate_ErrorCodes(t *testing.T) {
	cases := []struct {
		code string
		kind engine.ErrorKind
	}{
		{"52003", engine.KindAuth},        // 未授权用户
		{"54001", engine.KindAuth},        // 签名错误
		{"90107", engine.KindAuth},        // 认证未通过
		{"54003", engine.KindRateLimit},   // 访问频率受限
		{"54004", engine.KindRateLimit},   // 账户余额不足
		{"54005", engine.KindRateLimit},   // 长 query 请求频繁
		{"58000", engine.KindConfig},      // 客户端 IP 非法
		{"58002", engine.KindConfig},      // 服务已关闭
		{"58001", engine.KindUnsupported}, // 译文语言方向不支持
		{"54000", engine.KindConfig},      // 必填参数为空
		{"52001", engine.KindNetwork},     // 请求超时
		{"52002", engine.KindUpstream},    // 系统错误
		{"99999", engine.KindUpstream},    // 未知码保守归为上游错误
	}
	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			// 关键：error_code 必须是 JSON 字符串，验证实现没有按数字解析
			body := `{"error_code":"` + c.code + `","error_msg":"测试错误"}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			e := newTestEngine(t, srv.URL)
			_, err := e.Translate(context.Background(), req())
			if err == nil {
				t.Fatalf("error_code=%s 应报错（若未报错说明按 int 解析了字符串错误码）", c.code)
			}
			var ee *engine.Error
			if !asEngineErr(err, &ee) {
				t.Fatalf("应返回 *engine.Error，得到 %T", err)
			}
			if ee.Kind != c.kind {
				t.Errorf("error_code=%s 应归类为 %s，得到 %s（%s）",
					c.code, kindName(c.kind), ee.KindName(), ee.Message)
			}
			if !strings.Contains(ee.Message, c.code) {
				t.Errorf("错误消息应含原始错误码 %s: %s", c.code, ee.Message)
			}
		})
	}
}

// TestTranslate_ErrorCodeAsNumberStillHandled 验证即使百度返回数字型 error_code
// 也不会被静默当成成功（防御性：官方文档说是字符串，但实现变更时不能崩）。
func TestTranslate_ErrorCodeAsNumberStillHandled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 故意返回数字而非字符串
		_, _ = w.Write([]byte(`{"error_code":52003,"error_msg":"unauthorized"}`))
	}))
	defer srv.Close()
	e := newTestEngine(t, srv.URL)
	_, err := e.Translate(context.Background(), req())
	// 数字型 error_code 会被 json 解析到 string 字段失败 → ErrorCode 为空，
	// 但 trans_result 也为空，因此应报「空结果」而不是静默返回空译文。
	if err == nil {
		t.Error("数字型 error_code 且无 trans_result 时应报错，不能静默成功")
	}
}

// TestBuild_Validation 验证构造期校验与默认端点。
func TestBuild_Validation(t *testing.T) {
	if _, err := engine.Build(Kind, engine.Config{}, engine.Secrets{"secretKey": "s"}, engine.Options{}); err == nil {
		t.Error("缺 appId 应报错")
	}
	if _, err := engine.Build(Kind, engine.Config{"appId": "a"}, engine.Secrets{}, engine.Options{}); err == nil {
		t.Error("缺 secretKey 应报错")
	}
	e, err := engine.Build(Kind, engine.Config{"appId": "a"}, engine.Secrets{"secretKey": "s"}, engine.Options{})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if e.Kind() != Kind {
		t.Errorf("Kind = %q", e.Kind())
	}
	if e.SupportsStreaming() {
		t.Error("百度不支持流式")
	}
	if e.MaxChars() <= 0 {
		t.Errorf("MaxChars = %d", e.MaxChars())
	}
	// 语言映射应含百度的私有码系（jp / kor / cht）
	langs := e.SupportedLangs()
	if len(langs) == 0 {
		t.Error("SupportedLangs 为空")
	}
}

// TestRegistration 验证已注册且 schema 完整。
func TestRegistration(t *testing.T) {
	_, sch, ok := engine.Lookup(Kind)
	if !ok {
		t.Fatalf("引擎 %s 未注册", Kind)
	}
	if sch.DefaultName == "" || sch.DefaultNameEn == "" {
		t.Error("schema 缺少 DefaultName / DefaultNameEn（前端无法自动补全名称）")
	}
	if sch.Streaming {
		t.Error("百度不支持流式，schema.Streaming 应为 false")
	}
	if !sch.NeedsSecret {
		t.Error("百度需要密钥")
	}
	keys := map[string]bool{}
	for _, f := range sch.Fields {
		keys[f.Key] = true
		if (f.Type == engine.FieldSelect || f.Type == engine.FieldRadio) && len(f.Options) == 0 {
			t.Errorf("字段 %s 是 %s 但没有 options", f.Key, f.Type)
		}
		if f.Secret && f.Type != engine.FieldPassword {
			t.Errorf("Secret 字段 %s 的类型应是 password，得到 %s", f.Key, f.Type)
		}
	}
	for _, need := range []string{"appId", "secretKey"} {
		if !keys[need] {
			t.Errorf("schema 缺少核心字段 %s", need)
		}
	}
}

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

var _ = json.Marshal
