package google

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gaogao/translation/engine"
)

const testKey = "AIzaTestKeyXXXXXXXXXXXXXXXXXXXXXXX"

func newTestEngine(t *testing.T, endpoint string) engine.Engine {
	t.Helper()
	e, err := engine.Build(Kind,
		engine.Config{"endpoint": endpoint},
		engine.Secrets{"apiKey": testKey},
		engine.Options{Name: "测试Google"})
	if err != nil {
		t.Fatalf("构造引擎失败: %v", err)
	}
	return e
}

func req() engine.Request {
	return engine.Request{Source: "hello world", SourceLang: "en", TargetLang: "zh"}
}

// TestTranslate_KeyInQueryParam 验证 Key 走【查询参数】而非 Authorization 头。
//
// Google Cloud Translation v2 的约定是 ?key=<API_KEY>。
// 放请求头会得到 403，而错误信息只说 "The request is missing a valid API key"，
// 完全不会提示你是放错了位置 —— 用户会以为 Key 本身无效。
func TestTranslate_KeyInQueryParam(t *testing.T) {
	var gotQueryKey, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQueryKey = r.URL.Query().Get("key")
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"data":{"translations":[{"translatedText":"你好世界","detectedSourceLanguage":"en"}]}}`))
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
	if gotQueryKey != testKey {
		t.Errorf("查询参数 key = %q, 期望 %q", gotQueryKey, testKey)
	}
	if gotAuth != "" {
		t.Errorf("Google v2 不用 Authorization 头，实际发了 %q", gotAuth)
	}
	// 请求体是 JSON，含 q（数组）、target、format
	if gotBody["target"] != "zh-CN" {
		t.Errorf("target = %v, 期望 zh-CN（Google 的简体中文码）", gotBody["target"])
	}
	if gotBody["source"] != "en" {
		t.Errorf("source = %v, 期望 en", gotBody["source"])
	}
	q, ok := gotBody["q"].([]any)
	if !ok || len(q) != 1 || q[0] != "hello world" {
		t.Errorf("q 应为单元素数组，得到 %v", gotBody["q"])
	}
}

// TestTranslate_HTMLUnescaped 验证译文被 HTML 反转义。
//
// Google 返回的 translatedText 做了 HTML 实体转义（' → &#39;、" → &quot;）。
// 不反转义的话用户会看到一堆 &#39;，尤其翻译含引号的句子时非常难看。
func TestTranslate_HTMLUnescaped(t *testing.T) {
	cases := []struct{ escaped, want string }{
		{"It&#39;s fine", "It's fine"},
		{"&quot;quoted&quot;", `"quoted"`},
		{"a &amp; b", "a & b"},
		{"&lt;tag&gt;", "<tag>"},
		{"你好", "你好"}, // 无实体时原样
	}
	for _, c := range cases {
		t.Run(c.escaped, func(t *testing.T) {
			resp := `{"data":{"translations":[{"translatedText":"` + c.escaped + `"}]}}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(resp))
			}))
			defer srv.Close()
			e := newTestEngine(t, srv.URL)
			res, err := e.Translate(context.Background(), req())
			if err != nil {
				t.Fatalf("失败: %v", err)
			}
			if res.Target != c.want {
				t.Errorf("反转义结果 = %q, 期望 %q", res.Target, c.want)
			}
		})
	}
}

// TestTranslate_AutoOmitsSource 验证 auto 时【不发】source 字段。
func TestTranslate_AutoOmitsSource(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"data":{"translations":[{"translatedText":"你好","detectedSourceLanguage":"en"}]}}`))
	}))
	defer srv.Close()
	e := newTestEngine(t, srv.URL)
	if _, err := e.Translate(context.Background(), engine.Request{
		Source: "hi", SourceLang: "auto", TargetLang: "zh",
	}); err != nil {
		t.Fatalf("失败: %v", err)
	}
	if _, exists := gotBody["source"]; exists {
		t.Errorf("auto 时不应发送 source 字段，实际发了 %v", gotBody["source"])
	}
}

// TestTranslate_DetectedLangToCanonical 验证检测到的语言转回规范码。
func TestTranslate_DetectedLangToCanonical(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"translations":[{"translatedText":"你好","detectedSourceLanguage":"en"}]}}`))
	}))
	defer srv.Close()
	e := newTestEngine(t, srv.URL)
	res, err := e.Translate(context.Background(), engine.Request{
		Source: "hi", SourceLang: "auto", TargetLang: "zh",
	})
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if res.DetectedLang != "en" {
		t.Errorf("DetectedLang = %q, 期望规范码 en", res.DetectedLang)
	}
}

// TestTranslate_InvalidKey400 验证「API key not valid」被归类为鉴权而非参数错误。
//
// Google 用 400 返回 Key 无效（不是 401/403），若按通用逻辑归类会落到
// KindConfig，提示用户「检查参数」—— 而真正该做的是换 Key。
func TestTranslate_InvalidKey400(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT"}}`))
	}))
	defer srv.Close()
	e := newTestEngine(t, srv.URL)
	_, err := e.Translate(context.Background(), req())
	var ee *engine.Error
	if !asEngineErr(err, &ee) {
		t.Fatalf("应返回 *engine.Error，得到 %T", err)
	}
	if ee.Kind != engine.KindAuth {
		t.Errorf("Key 无效应归类为 KindAuth，得到 %s（%s）", ee.KindName(), ee.Message)
	}
	if !strings.Contains(ee.Message, "API Key") {
		t.Errorf("错误消息应指向 Key 问题: %s", ee.Message)
	}
}

// TestTranslate_ErrorClassification 验证各类错误的归类。
func TestTranslate_ErrorClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		kind   engine.ErrorKind
	}{
		{"语言不支持", 400, `{"error":{"message":"Invalid language pair"}}`, engine.KindConfig},
		{"未启用 API", 403, `{"error":{"message":"Cloud Translation API has not been used"}}`, engine.KindAuth},
		{"频率限制", 429, `{"error":{"message":"Quota exceeded"}}`, engine.KindRateLimit},
		{"服务端错误", 500, `{"error":{"message":"backend error"}}`, engine.KindUpstream},
		{"服务不可用", 503, `{"error":{"message":"unavailable"}}`, engine.KindUpstream},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			e := newTestEngine(t, srv.URL)
			_, err := e.Translate(context.Background(), req())
			if err == nil {
				t.Fatalf("status=%d 应报错", c.status)
			}
			var ee *engine.Error
			if !asEngineErr(err, &ee) {
				t.Fatalf("应返回 *engine.Error，得到 %T", err)
			}
			if ee.Kind != c.kind {
				t.Errorf("应归类为 %s，得到 %s（%s）", kindName(c.kind), ee.KindName(), ee.Message)
			}
		})
	}
}

// TestTranslate_EmptyTranslations 验证空结果被识别。
func TestTranslate_EmptyTranslations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"translations":[]}}`))
	}))
	defer srv.Close()
	e := newTestEngine(t, srv.URL)
	_, err := e.Translate(context.Background(), req())
	var ee *engine.Error
	if !asEngineErr(err, &ee) || ee.Kind != engine.KindProtocol {
		t.Errorf("空 translations 应归类为 KindProtocol，得到 %v", err)
	}
}

// TestTranslate_InvalidLangRejected 验证非法语言码在本地被拦截，不发请求。
//
// Google 的映射表覆盖了全部 31 个规范语言，所以这里用一个【不在规范集里】
// 的码来验证拦截机制。若放过，上游会返回 400 "Invalid language pair"，
// 用户看到的是英文错误码，不如本地直接给出中文提示。
func TestTranslate_InvalidLangRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("非法语言码不应发出请求")
	}))
	defer srv.Close()
	e := newTestEngine(t, srv.URL)

	for _, bad := range []string{"klingon", "zh-LATN", "EN", ""} {
		_, err := e.Translate(context.Background(), engine.Request{
			Source: "hi", SourceLang: "en", TargetLang: bad,
		})
		if err == nil {
			t.Errorf("目标语言 %q 应被拒绝", bad)
			continue
		}
		var ee *engine.Error
		if !asEngineErr(err, &ee) {
			t.Errorf("应返回 *engine.Error，得到 %T", err)
			continue
		}
		// 空串是「缺参数」→ KindConfig；非法码是「不支持」→ KindUnsupported
		if ee.Kind != engine.KindUnsupported && ee.Kind != engine.KindConfig {
			t.Errorf("目标语言 %q 应归类为 Unsupported/Config，得到 %s", bad, ee.KindName())
		}
	}
}

// TestBuild_Validation 验证构造期校验与默认端点。
func TestBuild_Validation(t *testing.T) {
	if _, err := engine.Build(Kind, engine.Config{}, engine.Secrets{}, engine.Options{}); err == nil {
		t.Error("缺 API Key 应报错")
	}
	e, err := engine.Build(Kind, engine.Config{}, engine.Secrets{"apiKey": "k"}, engine.Options{})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if got := e.(*engineImpl).endpoint; !strings.Contains(got, "translation.googleapis.com") {
		t.Errorf("默认 endpoint = %q", got)
	}
	if e.SupportsStreaming() {
		t.Error("Google 翻译不支持流式")
	}
	if e.MaxChars() <= 0 {
		t.Errorf("MaxChars = %d", e.MaxChars())
	}
}

// TestStream_FallsBackToSingleChunk 验证单 chunk 降级。
func TestStream_FallsBackToSingleChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"translations":[{"translatedText":"你好世界"}]}}`))
	}))
	defer srv.Close()
	e := newTestEngine(t, srv.URL)
	var chunks []string
	res, err := e.Stream(context.Background(), req(), func(c engine.Chunk) error {
		chunks = append(chunks, c.Text)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream 失败: %v", err)
	}
	if len(chunks) != 1 || chunks[0] != "你好世界" {
		t.Errorf("应降级为单 chunk，得到 %v", chunks)
	}
	if res.Target != "你好世界" {
		t.Errorf("Target = %q", res.Target)
	}
}

// TestRegistration 验证注册与 schema 完整性。
func TestRegistration(t *testing.T) {
	_, sch, ok := engine.Lookup(Kind)
	if !ok {
		t.Fatalf("引擎 %s 未注册", Kind)
	}
	if sch.DefaultName == "" || sch.DefaultNameEn == "" {
		t.Error("schema 缺少 DefaultName / DefaultNameEn")
	}
	if sch.Streaming {
		t.Error("Google 翻译不支持流式，schema.Streaming 应为 false")
	}
	keys := map[string]bool{}
	for _, f := range sch.Fields {
		keys[f.Key] = true
		if f.Secret && f.Type != engine.FieldPassword {
			t.Errorf("Secret 字段 %s 类型应为 password", f.Key)
		}
	}
	if !keys["apiKey"] {
		t.Error("schema 缺少 apiKey 字段")
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
