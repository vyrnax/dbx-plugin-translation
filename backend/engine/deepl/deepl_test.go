package deepl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gaogao/translation/engine"
)

func newTestEngine(t *testing.T, endpoint, key string) engine.Engine {
	t.Helper()
	cfg := engine.Config{"serverKind": "free"}
	if endpoint != "" {
		cfg["endpoint"] = endpoint
	}
	e, err := engine.Build(Kind, cfg, engine.Secrets{"apiKey": key}, engine.Options{Name: "测试DeepL"})
	if err != nil {
		t.Fatalf("构造引擎失败: %v", err)
	}
	return e
}

func req() engine.Request {
	return engine.Request{Source: "hello world", SourceLang: "en", TargetLang: "zh"}
}

// TestTranslate_AuthHeader 验证 DeepL 用的是自定义 scheme 而非 Bearer。
//
// DeepL 要求 `Authorization: DeepL-Auth-Key <key>`。写成 Bearer 会返回 403，
// 而 403 的默认解释是「Key 无效」，用户会去检查 Key 而不是怀疑 scheme。
func TestTranslate_AuthHeader(t *testing.T) {
	var gotAuth, gotCT string
	var gotForm map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		_ = r.ParseForm()
		gotForm = r.PostForm
		_, _ = w.Write([]byte(`{"translations":[{"detected_source_language":"EN","text":"你好世界"}]}`))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL+"/v2/translate", "test-key:fx")
	res, err := e.Translate(context.Background(), req())
	if err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	if res.Target != "你好世界" {
		t.Errorf("译文 = %q", res.Target)
	}
	if gotAuth != "DeepL-Auth-Key test-key:fx" {
		t.Errorf("Authorization = %q, 期望 DeepL-Auth-Key 前缀", gotAuth)
	}
	if strings.HasPrefix(gotAuth, "Bearer") {
		t.Error("DeepL 不用 Bearer scheme")
	}
	if !strings.Contains(gotCT, "application/x-www-form-urlencoded") {
		t.Errorf("Content-Type = %q, DeepL 用表单编码而非 JSON", gotCT)
	}
	if gotForm["text"][0] != "hello world" {
		t.Errorf("text = %v", gotForm["text"])
	}
	if gotForm["target_lang"][0] != "ZH" {
		t.Errorf("target_lang = %v, 期望 ZH（DeepL 大写码）", gotForm["target_lang"])
	}
	// 【关键】source_lang 必须是 EN，不能是 EN-US。
	//
	// DeepL 的 source_lang 与 target_lang 接受的取值集合不同：
	//   target_lang 接受 EN-US / EN-GB（必须带地区后缀）
	//   source_lang 只接受 EN（不带地区后缀）
	//
	// 这条断言原先写的是 EN-US —— 也就是说【测试把 bug 当成了正确行为】，
	// 所以线上才会出现「用户填了正确的 Key，点测试连接报 400
	// Value for 'source_lang' not supported」。
	//
	// 教训：断言的值必须来自官方文档，不能来自"实现现在输出什么"。
	// 照着实现写断言，等于把 bug 固化成规范，测试永远绿、线上永远错。
	if gotForm["source_lang"][0] != "EN" {
		t.Errorf("source_lang = %v, 期望 EN（DeepL 的 source_lang 不接受地区后缀，"+
			"传 EN-US 会返回 400）", gotForm["source_lang"])
	}
}

// TestTranslate_TargetLangKeepsRegion 验证目标语言位置【仍带】地区后缀。
//
// 修 source_lang 时最容易顺手把 target_lang 也改成 EN —— 但 DeepL 的
// target_lang 要求明确地区变体，传 EN 会返回
// 400 "Value for 'target_lang' not supported"。
// 两个位置的规则相反，必须分别断言。
func TestTranslate_TargetLangKeepsRegion(t *testing.T) {
	cases := []struct{ target, want string }{
		{"en", "EN-US"}, // 英语必须带地区后缀
		{"pt", "PT-PT"}, // 葡萄牙语必须带地区后缀
		{"zh", "ZH"},    // 中文无地区差异
		{"ja", "JA"},
	}
	for _, c := range cases {
		t.Run(c.target, func(t *testing.T) {
			var gotForm map[string][]string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				gotForm = r.PostForm
				_, _ = w.Write([]byte(`{"translations":[{"text":"x"}]}`))
			}))
			defer srv.Close()

			e := newTestEngine(t, srv.URL+"/v2/translate", "k:fx")
			// 源语言固定用中文，避免与 target 相同
			if _, err := e.Translate(context.Background(), engine.Request{
				Source: "你好", SourceLang: "zh", TargetLang: c.target,
			}); err != nil {
				t.Fatalf("翻译失败: %v", err)
			}
			if got := gotForm["target_lang"][0]; got != c.want {
				t.Errorf("target_lang = %q, 期望 %q", got, c.want)
			}
			// 源语言位置：中文是 ZH，两个位置相同
			if got := gotForm["source_lang"][0]; got != "ZH" {
				t.Errorf("source_lang = %q, 期望 ZH", got)
			}
		})
	}
}

// TestTranslate_AutoOmitsSourceLang 验证 auto 时【不发】source_lang 参数。
//
// DeepL 用「省略 source_lang」表示自动检测。传空串会被判为非法参数（400）。
func TestTranslate_AutoOmitsSourceLang(t *testing.T) {
	var gotForm map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		_, _ = w.Write([]byte(`{"translations":[{"detected_source_language":"EN","text":"你好"}]}`))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL+"/v2/translate", "k:fx")
	if _, err := e.Translate(context.Background(), engine.Request{
		Source: "hi", SourceLang: "auto", TargetLang: "zh",
	}); err != nil {
		t.Fatalf("失败: %v", err)
	}
	if _, exists := gotForm["source_lang"]; exists {
		t.Errorf("auto 时不应发送 source_lang 参数，实际发了 %q", gotForm["source_lang"])
	}
}

// TestTranslate_DetectedLangToCanonical 验证检测到的语言码被转回规范码。
func TestTranslate_DetectedLangToCanonical(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"translations":[{"detected_source_language":"EN-US","text":"你好"}]}`))
	}))
	defer srv.Close()
	e := newTestEngine(t, srv.URL+"/v2/translate", "k:fx")
	res, err := e.Translate(context.Background(), engine.Request{
		Source: "hi", SourceLang: "auto", TargetLang: "zh",
	})
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if res.DetectedLang != "en" {
		t.Errorf("DetectedLang = %q, 期望规范码 en（而非 DeepL 的 EN-US）", res.DetectedLang)
	}
}

// TestToCanonical_BothForms 验证 DeepL 私有码转规范码时【两种形态都能匹配】。
//
// DeepL 的 detected_source_language 返回源语言形态（英语是 EN），
// 而 lang.Map 返回目标形态（EN-US）。只比对目标形态的话英语永远匹配不上，
// detectedLang 会一直是空 —— 这是引入 sourceOverrides 后新产生的不一致，
// 已在线上验证中发现（英译中成功但 detectedLang 为空）。
func TestToCanonical_BothForms(t *testing.T) {
	cases := []struct{ deeplCode, want string }{
		// 源语言形态（detected_source_language 实际返回的）
		{"EN", "en"},
		{"PT", "pt"},
		// 目标语言形态（带地区后缀）
		{"EN-US", "en"},
		{"PT-PT", "pt"},
		// 无地区差异的语言
		{"ZH", "zh"},
		{"JA", "ja"},
		{"DE", "de"},
		{"NB", "no"},
		// 大小写不敏感
		{"en", "en"},
		{"zh", "zh"},
		// 空值与未知码
		{"", ""},
		{"   ", ""},
		{"XX", ""},
	}
	for _, c := range cases {
		if got := toCanonical(c.deeplCode); got != c.want {
			t.Errorf("toCanonical(%q) = %q, 期望 %q", c.deeplCode, got, c.want)
		}
	}
}

// TestTranslate_DetectedLangFromSourceForm 验证真实响应里的
// detected_source_language（源形态 EN）能被正确转换。
func TestTranslate_DetectedLangFromSourceForm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// DeepL 真实返回的就是源形态的 EN，不是 EN-US
		_, _ = w.Write([]byte(`{"translations":[{"detected_source_language":"EN","text":"你好"}]}`))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL+"/v2/translate", "k:fx")
	res, err := e.Translate(context.Background(), engine.Request{
		Source: "hi", SourceLang: "auto", TargetLang: "zh",
	})
	if err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	if res.DetectedLang != "en" {
		t.Errorf("DetectedLang = %q, 期望 en（源形态 EN 必须能转回规范码）", res.DetectedLang)
	}
}

// TestTranslate_QuotaExhausted456 验证 DeepL 专有的 456 状态码。
//
// 456 不是标准 HTTP 码，DeepL 用它表示「配额耗尽」。
// 若按通用逻辑归类，会落到「上游错误」并提示重试 —— 但重试没用，
// 用户需要知道是配额问题（升级套餐或等下个周期）。
func TestTranslate_QuotaExhausted456(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(456)
		_, _ = w.Write([]byte(`{"message":"Quota exceeded"}`))
	}))
	defer srv.Close()
	e := newTestEngine(t, srv.URL+"/v2/translate", "k:fx")
	_, err := e.Translate(context.Background(), req())
	var ee *engine.Error
	if !asEngineErr(err, &ee) {
		t.Fatalf("应返回 *engine.Error，得到 %T", err)
	}
	if ee.Kind != engine.KindRateLimit {
		t.Errorf("456 应归类为 KindRateLimit，得到 %s", ee.KindName())
	}
	if ee.Retryable() {
		t.Error("配额耗尽不可重试（重试也没配额）")
	}
	if !strings.Contains(ee.Message, "配额") {
		t.Errorf("错误消息应说明是配额问题: %s", ee.Message)
	}
}

// TestTranslate_WrongEndpoint403 验证免费 Key 请求付费端点时的 403 提示。
func TestTranslate_WrongEndpoint403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Wrong endpoint"}`))
	}))
	defer srv.Close()
	e := newTestEngine(t, srv.URL+"/v2/translate", "k:fx")
	_, err := e.Translate(context.Background(), req())
	var ee *engine.Error
	if !asEngineErr(err, &ee) {
		t.Fatalf("应返回 *engine.Error")
	}
	if ee.Kind != engine.KindAuth {
		t.Errorf("403 应归类为 KindAuth，得到 %s", ee.KindName())
	}
	// 错误消息应提示端点问题，而不只是「Key 无效」
	if !strings.Contains(ee.Message, "端点") {
		t.Errorf("403 消息应提示可能是端点用错: %s", ee.Message)
	}
}

// TestTranslate_RateLimit429 验证 429 归类为限流。
func TestTranslate_RateLimit429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	e := newTestEngine(t, srv.URL+"/v2/translate", "k:fx")
	_, err := e.Translate(context.Background(), req())
	var ee *engine.Error
	if !asEngineErr(err, &ee) || ee.Kind != engine.KindRateLimit {
		t.Errorf("429 应归类为 KindRateLimit，得到 %v", err)
	}
}

// TestTranslate_EmptyTranslations 验证空结果被识别。
func TestTranslate_EmptyTranslations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"translations":[]}`))
	}))
	defer srv.Close()
	e := newTestEngine(t, srv.URL+"/v2/translate", "k:fx")
	_, err := e.Translate(context.Background(), req())
	var ee *engine.Error
	if !asEngineErr(err, &ee) || ee.Kind != engine.KindProtocol {
		t.Errorf("空 translations 应归类为 KindProtocol，得到 %v", err)
	}
}

// TestTranslate_FormalityAndGlossary 验证可选参数透传。
func TestTranslate_FormalityAndGlossary(t *testing.T) {
	var gotForm map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		_, _ = w.Write([]byte(`{"translations":[{"text":"你好"}]}`))
	}))
	defer srv.Close()

	e, err := engine.Build(Kind,
		engine.Config{"endpoint": srv.URL + "/v2/translate", "formality": "more", "glossaryId": "gloss-123"},
		engine.Secrets{"apiKey": "k:fx"}, engine.Options{})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	// 使用术语表时必须显式指定源语言
	if _, err := e.Translate(context.Background(), req()); err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	if gotForm["formality"][0] != "more" {
		t.Errorf("formality = %v", gotForm["formality"])
	}
	if gotForm["glossary_id"][0] != "gloss-123" {
		t.Errorf("glossary_id = %v", gotForm["glossary_id"])
	}
}

// TestTranslate_GlossaryRequiresSourceLang 验证术语表 + auto 源语言被拒绝。
//
// DeepL 硬性要求：使用 glossary_id 时必须显式指定 source_lang，
// 否则返回 400 "glossary_id requires source_lang"。提前拦截能给出更清楚的提示。
func TestTranslate_GlossaryRequiresSourceLang(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("不应发出请求：术语表 + auto 应在本地被拦截")
	}))
	defer srv.Close()

	e, err := engine.Build(Kind,
		engine.Config{"endpoint": srv.URL + "/v2/translate", "glossaryId": "gloss-123"},
		engine.Secrets{"apiKey": "k:fx"}, engine.Options{})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	_, err = e.Translate(context.Background(), engine.Request{
		Source: "hi", SourceLang: "auto", TargetLang: "zh",
	})
	if err == nil {
		t.Fatal("术语表 + auto 源语言应被拒绝")
	}
	var ee *engine.Error
	if !asEngineErr(err, &ee) || ee.Kind != engine.KindConfig {
		t.Errorf("应归类为 KindConfig，得到 %v", err)
	}
	if !strings.Contains(ee.Message, "源语言") {
		t.Errorf("错误消息应说明需要指定源语言: %s", ee.Message)
	}
}

// TestBuild_KeyEndpointMismatch 验证免费/付费 Key 与端点的匹配性检查。
//
// DeepL 免费 Key 以 ":fx" 结尾，只能配 api-free.deepl.com。
// 用错会 403 且错误消息不说明原因，提前拦截能省一次往返。
func TestBuild_KeyEndpointMismatch(t *testing.T) {
	// 免费 Key + 付费端点 → 拒绝
	_, err := engine.Build(Kind,
		engine.Config{"serverKind": "pro"},
		engine.Secrets{"apiKey": "free-key:fx"}, engine.Options{})
	if err == nil {
		t.Error("免费 Key 配付费端点应被拒绝")
	}
	// 付费 Key + 免费端点 → 拒绝
	_, err = engine.Build(Kind,
		engine.Config{"serverKind": "free"},
		engine.Secrets{"apiKey": "paid-key-no-suffix"}, engine.Options{})
	if err == nil {
		t.Error("付费 Key 配免费端点应被拒绝")
	}
	// 免费 Key + 免费端点 → 通过
	if _, err := engine.Build(Kind,
		engine.Config{"serverKind": "free"},
		engine.Secrets{"apiKey": "free-key:fx"}, engine.Options{}); err != nil {
		t.Errorf("免费 Key 配免费端点应通过: %v", err)
	}
	// 付费 Key + 付费端点 → 通过
	if _, err := engine.Build(Kind,
		engine.Config{"serverKind": "pro"},
		engine.Secrets{"apiKey": "paid-key"}, engine.Options{}); err != nil {
		t.Errorf("付费 Key 配付费端点应通过: %v", err)
	}
}

// TestBuild_EndpointAutoComplete 验证只填域名时自动补全路径。
func TestBuild_EndpointAutoComplete(t *testing.T) {
	e, err := engine.Build(Kind,
		engine.Config{"endpoint": "https://my-proxy.example.com"},
		engine.Secrets{"apiKey": "k"}, engine.Options{})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if got := e.(*engineImpl).endpoint; !strings.HasSuffix(got, "/v2/translate") {
		t.Errorf("endpoint = %q, 应自动补全 /v2/translate", got)
	}
}

// TestBuild_MissingKey 验证缺 Key 被拒绝。
func TestBuild_MissingKey(t *testing.T) {
	if _, err := engine.Build(Kind, engine.Config{}, engine.Secrets{}, engine.Options{}); err == nil {
		t.Error("缺 API Key 应报错")
	}
}

// TestStream_FallsBackToSingleChunk 验证不支持流式的引擎走单 chunk 降级。
func TestStream_FallsBackToSingleChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"translations":[{"text":"你好世界"}]}`))
	}))
	defer srv.Close()
	e := newTestEngine(t, srv.URL+"/v2/translate", "k:fx")
	if e.SupportsStreaming() {
		t.Error("DeepL 不应声明支持流式")
	}
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
		t.Error("DeepL 不支持流式，schema.Streaming 应为 false")
	}
	keys := map[string]bool{}
	for _, f := range sch.Fields {
		keys[f.Key] = true
		if (f.Type == engine.FieldSelect || f.Type == engine.FieldRadio) && len(f.Options) == 0 {
			t.Errorf("字段 %s 是 %s 但没有 options", f.Key, f.Type)
		}
	}
	for _, need := range []string{"apiKey", "serverKind"} {
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
