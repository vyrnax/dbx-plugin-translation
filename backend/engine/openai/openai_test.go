package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vyrnax/translation/engine"
)

// newTestEngine 构造一个指向 httptest 服务器的引擎实例。
func newTestEngine(t *testing.T, url string, cfg engine.Config) engine.Engine {
	t.Helper()
	if cfg == nil {
		cfg = engine.Config{}
	}
	if _, ok := cfg["endpoint"]; !ok {
		cfg["endpoint"] = url
	}
	if _, ok := cfg["model"]; !ok {
		cfg["model"] = "test-model"
	}
	e, err := engine.Build(Kind, cfg, engine.Secrets{"apiKey": "sk-test-key"}, engine.Options{Name: "测试引擎"})
	if err != nil {
		t.Fatalf("构造引擎失败: %v", err)
	}
	return e
}

func baseRequest(text string) engine.Request {
	return engine.Request{Source: text, SourceLang: "en", TargetLang: "zh"}
}

// sseEvent 构造一个 SSE data 行。
func sseEvent(content string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"delta": map[string]any{"content": content}}},
	})
	return "data: " + string(b) + "\n\n"
}

// ---------------------------------------------------------------------------
// 端点归一化
// ---------------------------------------------------------------------------

// TestNormalizeEndpoint 验证用户填写的各种形态都能补全成完整路径。
//
// 这是最高频的配置错误来源：用户填错路径的症状是 404，
// 而 404 的响应体通常是服务商的 HTML 页面，用户完全看不出是路径问题。
func TestNormalizeEndpoint(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://api.deepseek.com", "https://api.deepseek.com/v1/chat/completions"},
		{"https://api.deepseek.com/", "https://api.deepseek.com/v1/chat/completions"},
		{"https://api.deepseek.com/v1", "https://api.deepseek.com/v1/chat/completions"},
		{"https://api.deepseek.com/v1/", "https://api.deepseek.com/v1/chat/completions"},
		{"https://api.deepseek.com/v1/chat/completions", "https://api.deepseek.com/v1/chat/completions"},
		{"http://127.0.0.1:11434", "http://127.0.0.1:11434/v1/chat/completions"},
		{"http://127.0.0.1:11434/v1", "http://127.0.0.1:11434/v1/chat/completions"},
		// 老式 completions 端点保持原样
		{"https://api.example.com/v1/completions", "https://api.example.com/v1/completions"},
	}
	for _, c := range cases {
		got, err := normalizeEndpoint(c.in)
		if err != nil {
			t.Errorf("normalizeEndpoint(%q) 报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("normalizeEndpoint(%q)\n  得: %s\n  期望: %s", c.in, got, c.want)
		}
	}
	if _, err := normalizeEndpoint(""); err == nil {
		t.Error("空地址应报错")
	}
	if _, err := normalizeEndpoint("   "); err == nil {
		t.Error("纯空白地址应报错")
	}
}

// TestNormalizeEndpoint_PreservesQuery 验证 query 参数被保留（Azure OpenAI 需要 api-version）。
func TestNormalizeEndpoint_PreservesQuery(t *testing.T) {
	in := "https://x.openai.azure.com/openai/deployments/gpt4?api-version=2024-01-01"
	got, err := normalizeEndpoint(in)
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if !strings.Contains(got, "api-version=2024-01-01") {
		t.Errorf("query 丢失: %s", got)
	}
	if !strings.HasSuffix(got, "?api-version=2024-01-01") {
		t.Errorf("query 应在末尾: %s", got)
	}
}

// ---------------------------------------------------------------------------
// 非流式翻译
// ---------------------------------------------------------------------------

// TestTranslate_Success 验证正常翻译路径与请求构造。
func TestTranslate_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("方法 = %s, 期望 POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test-key" {
			t.Errorf("Authorization = %q", got)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		var body chatRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("请求体解析失败: %v", err)
		}
		if body.Model != "test-model" {
			t.Errorf("model = %q", body.Model)
		}
		if body.Stream {
			t.Error("非流式调用不应设 stream=true")
		}
		if len(body.Messages) < 2 {
			t.Errorf("消息数 = %d, 期望至少 2（system + user）", len(body.Messages))
		}
		if body.Messages[0].Role != "system" {
			t.Errorf("首条消息角色 = %q, 期望 system", body.Messages[0].Role)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"content": "你好，世界"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL, nil)
	res, err := e.Translate(context.Background(), baseRequest("hello world"))
	if err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	if res.Target != "你好，世界" {
		t.Errorf("译文 = %q", res.Target)
	}
	if res.Chunks != 1 {
		t.Errorf("Chunks = %d, 期望 1", res.Chunks)
	}
	if res.Model != "test-model" {
		t.Errorf("Model = %q", res.Model)
	}
	// 耗时只断言【非负】，绝不断言「必须为正」。
	//
	// 原先这里写的是 `ElapsedMs <= 0` 报错，即假设「一次翻译至少花 1ms」。
	// 这个假设在快机器上不成立，导致 CI 随机失败（同一份代码前一次 run 还是绿的）：
	//   - Linux CI：时钟细但机器快，对进程内 httptest 端点的一次往返常在 0.x ms 内完成；
	//   - Windows 本机：Go 的时钟粒度更粗，实测连续两次 time.Now() 有 100% 概率返回
	//     完全相同的值，200 次进程内 HTTP 往返里 177 次 time.Since(start) 直接等于 0。
	// 也就是说「耗时 0ms」是【合法状态】，不是缺陷。
	//
	// 教训（对齐 AGENTS.md 6.8）：断言的值必须来自规范，不能来自「实现现在输出什么」。
	// 把机器速度当成规范的一部分，测试就必然 flaky。
	// 真正需要保证的是「不返回负数」（时钟回拨时不得显示 -3ms），见 engine.ElapsedMs。
	if res.ElapsedMs < 0 {
		t.Errorf("ElapsedMs = %d, 不得为负数", res.ElapsedMs)
	}
}

// TestTranslate_AuthFailure 验证 401/403 归类为 KindAuth 且【不可重试】。
//
// 重试鉴权失败必然再失败，还白白消耗配额；某些服务商甚至会因频繁失败临时封禁。
func TestTranslate_AuthFailure(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		func() {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided"}}`))
			}))
			defer srv.Close()

			e := newTestEngine(t, srv.URL, nil)
			_, err := e.Translate(context.Background(), baseRequest("hi"))
			if err == nil {
				t.Errorf("%d 应报错", status)
				return
			}
			var ee *engine.Error
			if !errors.As(err, &ee) {
				t.Errorf("%d 应返回 *engine.Error，得到 %T", status, err)
				return
			}
			if ee.Kind != engine.KindAuth {
				t.Errorf("%d 应归类为 KindAuth，得到 %s", status, ee.KindName())
			}
			if ee.Code() != -32031 {
				t.Errorf("%d 错误码 = %d, 期望 -32031", status, ee.Code())
			}
			if ee.Retryable() {
				t.Errorf("%d 鉴权失败绝不可自动重试", status)
			}
			// 服务商的错误消息应被提取出来展示给用户
			if !strings.Contains(ee.Message, "Incorrect API key") {
				t.Errorf("%d 未提取服务商错误消息: %s", status, ee.Message)
			}
		}()
	}
}

// TestTranslate_RateLimit 验证 429 归类为 KindRateLimit 且不可重试。
func TestTranslate_RateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"Rate limit reached"}}`))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL, nil)
	_, err := e.Translate(context.Background(), baseRequest("hi"))
	var ee *engine.Error
	if !errors.As(err, &ee) {
		t.Fatalf("应返回 *engine.Error，得到 %T", err)
	}
	if ee.Kind != engine.KindRateLimit {
		t.Errorf("应归类为 KindRateLimit，得到 %s", ee.KindName())
	}
	if ee.Code() != -32032 {
		t.Errorf("错误码 = %d, 期望 -32032", ee.Code())
	}
	if ee.Retryable() {
		t.Error("限流不可自动重试（会加重限流甚至触发封禁）")
	}
}

// TestTranslate_QuotaExhausted 验证「余额不足」被识别为配额问题而非鉴权问题。
//
// 服务商通常用 402/403 返回余额不足，但用户看到「鉴权失败」会去检查 Key，
// 而真正的问题是账户没钱 —— 错误分类错误会把用户引向完全错误的排查方向。
func TestTranslate_QuotaExhausted(t *testing.T) {
	for _, msg := range []string{"You exceeded your current quota", "账户余额不足", "insufficient balance"} {
		func() {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{"message": msg},
				})
			}))
			defer srv.Close()

			e := newTestEngine(t, srv.URL, nil)
			_, err := e.Translate(context.Background(), baseRequest("hi"))
			var ee *engine.Error
			if !errors.As(err, &ee) {
				t.Errorf("%q: 应返回 *engine.Error", msg)
				return
			}
			if ee.Kind != engine.KindRateLimit {
				t.Errorf("%q: 余额不足应归类为 KindRateLimit，得到 %s", msg, ee.KindName())
			}
		}()
	}
}

// TestTranslate_UpstreamError 验证 5xx 归类为 KindUpstream 且【可重试】。
func TestTranslate_UpstreamError(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504} {
		func() {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`upstream failure`))
			}))
			defer srv.Close()

			e := newTestEngine(t, srv.URL, nil)
			_, err := e.Translate(context.Background(), baseRequest("hi"))
			var ee *engine.Error
			if !errors.As(err, &ee) {
				t.Errorf("%d 应返回 *engine.Error", status)
				return
			}
			if ee.Kind != engine.KindUpstream {
				t.Errorf("%d 应归类为 KindUpstream，得到 %s", status, ee.KindName())
			}
			if !ee.Retryable() {
				t.Errorf("%d 上游错误应可重试", status)
			}
		}()
	}
}

// TestTranslate_InvalidJSON 验证非法 JSON 归类为 KindProtocol 并附诊断片段。
func TestTranslate_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices": [这不是合法JSON`))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL, nil)
	_, err := e.Translate(context.Background(), baseRequest("hi"))
	var ee *engine.Error
	if !errors.As(err, &ee) {
		t.Fatalf("应返回 *engine.Error，得到 %T", err)
	}
	if ee.Kind != engine.KindProtocol {
		t.Errorf("应归类为 KindProtocol，得到 %s", ee.KindName())
	}
	if ee.Code() != -32033 {
		t.Errorf("错误码 = %d, 期望 -32033", ee.Code())
	}
	if ee.Detail == "" {
		t.Error("应附带诊断片段（服务商改 API 时这是唯一线索）")
	}
}

// TestTranslate_EmptyChoices 验证空 choices 的处理。
// 模型名不存在时，某些服务商会返回 200 + 空 choices 而不是 4xx。
func TestTranslate_EmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL, nil)
	_, err := e.Translate(context.Background(), baseRequest("hi"))
	var ee *engine.Error
	if !errors.As(err, &ee) {
		t.Fatalf("应返回 *engine.Error，得到 %T", err)
	}
	if ee.Kind != engine.KindProtocol {
		t.Errorf("应归类为 KindProtocol，得到 %s", ee.KindName())
	}
	if !strings.Contains(ee.Message, "choices") {
		t.Errorf("错误消息应提到 choices: %s", ee.Message)
	}
}

// TestTranslate_EmptyContent 验证空译文的处理。
func TestTranslate_EmptyContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"   "},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL, nil)
	_, err := e.Translate(context.Background(), baseRequest("hi"))
	if err == nil {
		t.Fatal("空译文应报错")
	}
	var ee *engine.Error
	if !errors.As(err, &ee) || ee.Kind != engine.KindProtocol {
		t.Errorf("应归类为 KindProtocol，得到 %v", err)
	}
}

// TestTranslate_TruncatedByLength 验证 finish_reason=length 被识别为截断。
//
// 静默返回半截译文比报错更糟：用户会以为翻译完成了，
// 拿着不完整的译文去用，问题在下游才暴露。
func TestTranslate_TruncatedByLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"半截译文"},"finish_reason":"length"}]}`))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL, nil)
	_, err := e.Translate(context.Background(), baseRequest("very long text"))
	var ee *engine.Error
	if !errors.As(err, &ee) {
		t.Fatalf("应返回 *engine.Error，得到 %T", err)
	}
	if ee.Kind != engine.KindConfig {
		t.Errorf("截断应归类为 KindConfig（提示用户调低上限），得到 %s", ee.KindName())
	}
	if !strings.Contains(ee.Message, "截断") {
		t.Errorf("错误消息应说明是截断: %s", ee.Message)
	}
}

// TestTranslate_HTMLErrorPage 验证服务商返回 HTML 错误页时不把它塞进错误消息。
//
// 网关错误常返回几 KB 的 HTML，全塞进 Detail 会让诊断报告不可读。
func TestTranslate_HTMLErrorPage(t *testing.T) {
	html := "<html><head><title>502 Bad Gateway</title></head><body>" +
		strings.Repeat("<p>nginx error detail</p>", 200) + "</body></html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(html))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL, nil)
	_, err := e.Translate(context.Background(), baseRequest("hi"))
	var ee *engine.Error
	if !errors.As(err, &ee) {
		t.Fatalf("应返回 *engine.Error，得到 %T", err)
	}
	if len([]rune(ee.Message)) > 300 {
		t.Errorf("错误消息过长（%d 字符），HTML 不应被整体塞入", len([]rune(ee.Message)))
	}
	if strings.Contains(ee.Message, "<html>") {
		t.Error("错误消息不应含 HTML 标签")
	}
}

// TestTranslate_Timeout 验证超时归类为 KindNetwork 且可重试。
func TestTranslate_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"太晚了"}}]}`))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL, engine.Config{"timeoutSecs": 5})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_, err := e.Translate(ctx, baseRequest("hi"))
	if err == nil {
		t.Fatal("超时应报错")
	}
	var ee *engine.Error
	if !errors.As(err, &ee) {
		t.Fatalf("应返回 *engine.Error，得到 %T", err)
	}
	if ee.Kind != engine.KindNetwork {
		t.Errorf("超时应归类为 KindNetwork，得到 %s", ee.KindName())
	}
	if !ee.Retryable() {
		t.Error("超时应可重试")
	}
}

// TestTranslate_Cancelled 验证 ctx 取消归类为 KindCancelled。
func TestTranslate_Cancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"x"}}]}`))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	_, err := e.Translate(ctx, baseRequest("hi"))
	if err == nil {
		t.Fatal("取消应报错")
	}
	var ee *engine.Error
	if !errors.As(err, &ee) {
		t.Fatalf("应返回 *engine.Error，得到 %T", err)
	}
	if ee.Kind != engine.KindCancelled {
		t.Errorf("取消应归类为 KindCancelled，得到 %s", ee.KindName())
	}
	if ee.Code() != -32034 {
		t.Errorf("错误码 = %d, 期望 -32034", ee.Code())
	}
	if ee.Retryable() {
		t.Error("用户主动取消不应被当作可重试错误")
	}
}

// TestTranslate_ConnectionRefused 验证连接被拒的错误消息可操作。
func TestTranslate_ConnectionRefused(t *testing.T) {
	e := newTestEngine(t, "http://127.0.0.1:1/v1/chat/completions", nil)
	_, err := e.Translate(context.Background(), baseRequest("hi"))
	var ee *engine.Error
	if !errors.As(err, &ee) {
		t.Fatalf("应返回 *engine.Error，得到 %T", err)
	}
	if ee.Kind != engine.KindNetwork {
		t.Errorf("应归类为 KindNetwork，得到 %s", ee.KindName())
	}
	// 错误消息必须给出可操作的建议，而不只是 "connection refused"
	if !strings.Contains(ee.Message, "连接被拒绝") && !strings.Contains(ee.Message, "网络") {
		t.Errorf("错误消息应可操作: %s", ee.Message)
	}
}

// ---------------------------------------------------------------------------
// 流式翻译
// ---------------------------------------------------------------------------

// TestStream_Success 验证正常流式：多个 chunk 按序 emit，最终结果完整。
func TestStream_Success(t *testing.T) {
	parts := []string{"你", "好", "，", "世", "界"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body chatRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !body.Stream {
			t.Error("流式调用应设 stream=true")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, p := range parts {
			_, _ = w.Write([]byte(sseEvent(p)))
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL, nil)
	var got []string
	var idx int
	res, err := e.Stream(context.Background(), baseRequest("hello world"), func(c engine.Chunk) error {
		got = append(got, c.Text)
		if c.Index != idx {
			t.Errorf("chunk 序号 = %d, 期望 %d", c.Index, idx)
		}
		idx++
		return nil
	})
	if err != nil {
		t.Fatalf("流式翻译失败: %v", err)
	}
	if strings.Join(got, "") != "你好，世界" {
		t.Errorf("增量拼接 = %q, 期望 你好，世界", strings.Join(got, ""))
	}
	if res.Target != "你好，世界" {
		t.Errorf("最终译文 = %q", res.Target)
	}
	if res.Chunks != len(parts) {
		t.Errorf("Chunks = %d, 期望 %d", res.Chunks, len(parts))
	}
	if len(got) != len(parts) {
		t.Errorf("emit 次数 = %d, 期望 %d", len(got), len(parts))
	}
}

// TestStream_MultiLineData 验证多行 data: 被正确合并后再解析。
//
// SSE 规范允许一个事件有多行 data，必须用 \n 连接后作为整体解析。
// 逐行独立解析会让每行都变成非法 JSON。
func TestStream_MultiLineData(t *testing.T) {
	raw := "data: {\"choices\":[{\"delta\":{\"content\":\n" +
		"data: \"合并成功\"}}]}\n\n" +
		"data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(raw))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL, nil)
	var got strings.Builder
	_, err := e.Stream(context.Background(), baseRequest("x"), func(c engine.Chunk) error {
		got.WriteString(c.Text)
		return nil
	})
	if err != nil {
		t.Fatalf("多行 data 应被合并解析，得到错误: %v", err)
	}
	if got.String() != "合并成功" {
		t.Errorf("结果 = %q, 期望 合并成功", got.String())
	}
}

// TestStream_HeartbeatComments 验证以 : 开头的心跳注释行被忽略。
//
// 不忽略的话它们会被当成 data 解析，产生大量无意义的解析失败。
func TestStream_HeartbeatComments(t *testing.T) {
	raw := ": keep-alive\n\n" +
		":\n\n" +
		sseEvent("正常") +
		": ping\n\n" +
		sseEvent("内容") +
		"data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(raw))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL, nil)
	var got strings.Builder
	_, err := e.Stream(context.Background(), baseRequest("x"), func(c engine.Chunk) error {
		got.WriteString(c.Text)
		return nil
	})
	if err != nil {
		t.Fatalf("心跳行不应导致失败: %v", err)
	}
	if got.String() != "正常内容" {
		t.Errorf("结果 = %q, 期望 正常内容", got.String())
	}
}

// TestStream_NoDoneMarker 验证流在没有 [DONE] 就关闭时仍能正常结束。
// 某些服务商（尤其是自建网关）不发结束标记就直接关连接。
func TestStream_NoDoneMarker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseEvent("内容")))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL, nil)
	var got strings.Builder
	res, err := e.Stream(context.Background(), baseRequest("x"), func(c engine.Chunk) error {
		got.WriteString(c.Text)
		return nil
	})
	if err != nil {
		t.Fatalf("缺少 [DONE] 不应导致失败: %v", err)
	}
	if got.String() != "内容" || res.Target != "内容" {
		t.Errorf("结果 = %q / %q", got.String(), res.Target)
	}
}

// TestStream_NoTrailingBlankLine 验证最后一个事件没有结尾空行时仍被处理。
func TestStream_NoTrailingBlankLine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"尾部\"}}]}"))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL, nil)
	var got strings.Builder
	_, err := e.Stream(context.Background(), baseRequest("x"), func(c engine.Chunk) error {
		got.WriteString(c.Text)
		return nil
	})
	if err != nil {
		t.Fatalf("缺少结尾空行不应导致失败: %v", err)
	}
	if got.String() != "尾部" {
		t.Errorf("结果 = %q, 期望 尾部", got.String())
	}
}

// TestStream_MalformedChunkSkipped 验证单个非法 JSON 块被跳过而不中断整个流。
//
// 某些服务商会在流里插入调试信息或非 JSON 行。
// 因为一个坏块就放弃整个翻译，用户体验远差于「跳过坏块继续」。
func TestStream_MalformedChunkSkipped(t *testing.T) {
	raw := sseEvent("正常一") +
		"data: 这不是JSON\n\n" +
		"data: {\"broken\":\n\n" +
		sseEvent("正常二") +
		"data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(raw))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL, nil)
	var got strings.Builder
	_, err := e.Stream(context.Background(), baseRequest("x"), func(c engine.Chunk) error {
		got.WriteString(c.Text)
		return nil
	})
	if err != nil {
		t.Fatalf("坏块应被跳过而非中断: %v", err)
	}
	if got.String() != "正常一正常二" {
		t.Errorf("结果 = %q, 期望 正常一正常二", got.String())
	}
}

// TestStream_EmptyStream 验证完全没有内容时报错而非返回空译文。
func TestStream_EmptyStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL, nil)
	_, err := e.Stream(context.Background(), baseRequest("x"), func(c engine.Chunk) error { return nil })
	var ee *engine.Error
	if !errors.As(err, &ee) {
		t.Fatalf("应返回 *engine.Error，得到 %T", err)
	}
	if ee.Kind != engine.KindProtocol {
		t.Errorf("空流应归类为 KindProtocol，得到 %s", ee.KindName())
	}
}

// TestStream_CancelStopsReading 是最关键的流式测试：
// emit 返回错误后必须【立即停止】读上游，否则白白消耗配额。
func TestStream_CancelStopsReading(t *testing.T) {
	var sent int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 1000; i++ {
			if _, err := w.Write([]byte(sseEvent(fmt.Sprintf("块%d", i)))); err != nil {
				return
			}
			atomic.AddInt32(&sent, 1)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL, nil)
	ctx, cancel := context.WithCancel(context.Background())
	emitted := 0
	_, err := e.Stream(ctx, baseRequest("x"), func(c engine.Chunk) error {
		emitted++
		if emitted >= 3 {
			cancel()
			return context.Canceled
		}
		return nil
	})
	if err == nil {
		t.Fatal("取消后应返回错误")
	}
	var ee *engine.Error
	if !errors.As(err, &ee) {
		t.Fatalf("应返回 *engine.Error，得到 %T", err)
	}
	if ee.Kind != engine.KindCancelled {
		t.Errorf("应归类为 KindCancelled，得到 %s", ee.KindName())
	}
	time.Sleep(300 * time.Millisecond)
	if n := atomic.LoadInt32(&sent); n > 50 {
		t.Errorf("取消后上游仍发送了 %d 个块，说明没有及时停止读取（浪费配额）", n)
	}
}

// TestStream_EmitErrorPropagates 验证 emit 返回的错误被原样传播。
func TestStream_EmitErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseEvent("a") + sseEvent("b") + "data: [DONE]\n\n"))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL, nil)
	sentinel := errors.New("前端已关闭")
	_, err := e.Stream(context.Background(), baseRequest("x"), func(c engine.Chunk) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("emit 的错误应被传播，得到 %v", err)
	}
}

// TestStream_AuthFailure 验证流式请求的鉴权失败也被正确分类。
//
// 流式路径的错误处理与非流式是【独立代码】，很容易漏掉一边的分类。
func TestStream_AuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL, nil)
	_, err := e.Stream(context.Background(), baseRequest("x"), func(c engine.Chunk) error { return nil })
	var ee *engine.Error
	if !errors.As(err, &ee) {
		t.Fatalf("应返回 *engine.Error，得到 %T", err)
	}
	if ee.Kind != engine.KindAuth {
		t.Errorf("流式 401 应归类为 KindAuth，得到 %s", ee.KindName())
	}
}

// TestStream_MessageFieldFallback 验证从 message.content 读取增量。
//
// 某些兼容实现在流式响应里用 message 而非 delta 字段。
func TestStream_MessageFieldFallback(t *testing.T) {
	raw := "data: {\"choices\":[{\"message\":{\"content\":\"回退\"}}]}\n\ndata: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(raw))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL, nil)
	var got strings.Builder
	_, err := e.Stream(context.Background(), baseRequest("x"), func(c engine.Chunk) error {
		got.WriteString(c.Text)
		return nil
	})
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if got.String() != "回退" {
		t.Errorf("结果 = %q, 期望 回退", got.String())
	}
}

// TestStream_CleansOutput 验证流式结果也经过 cleanTranslation 清洗。
//
// 流式与非流式是两条独立代码路径，很容易只在一条上做清洗。
func TestStream_CleansOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseEvent("译文：") + sseEvent("你好") + "data: [DONE]\n\n"))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL, nil)
	res, err := e.Stream(context.Background(), baseRequest("x"), func(c engine.Chunk) error { return nil })
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if res.Target != "你好" {
		t.Errorf("流式结果未清洗: %q, 期望 你好", res.Target)
	}
}

// ---------------------------------------------------------------------------
// SSE 行解析
// ---------------------------------------------------------------------------

// TestParseSSELine 验证「冒号后第一个空格属于分隔符，其余空格是值的一部分」。
//
// 译文里的空格很常见，多去一个就会让英文单词粘连。
func TestParseSSELine(t *testing.T) {
	cases := []struct{ in, field, value string }{
		{"data: hello", "data", "hello"},
		{"data:hello", "data", "hello"},
		{"data:  两个空格", "data", " 两个空格"},
		{"data: hello world", "data", "hello world"},
		{"data:", "data", ""},
		{"event: message", "event", "message"},
		{"nodata", "nodata", ""},
		{`data: {"a":"b:c"}`, "data", `{"a":"b:c"}`},
	}
	for _, c := range cases {
		f, v := parseSSELine(c.in)
		if f != c.field || v != c.value {
			t.Errorf("parseSSELine(%q) = (%q,%q), 期望 (%q,%q)", c.in, f, v, c.field, c.value)
		}
	}
}

// TestSSEScanner_EventBoundaries 验证事件以空行分隔、多行 data 合并。
func TestSSEScanner_EventBoundaries(t *testing.T) {
	raw := "data: a\n" +
		"data: b\n" +
		"\n" +
		": 心跳\n" +
		"\n" +
		"data: c\n" +
		"\n"
	sc := newSSEScanner(strings.NewReader(raw))
	var events []string
	for sc.Scan() {
		if d, ok := sc.Data(); ok {
			events = append(events, d)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("扫描出错: %v", err)
	}
	want := []string{"a\nb", "c"}
	if len(events) != len(want) {
		t.Fatalf("事件数 = %d, 期望 %d: %v", len(events), len(want), events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Errorf("事件 %d = %q, 期望 %q", i, events[i], want[i])
		}
	}
}

// TestSSEScanner_FlushesTrailingEvent 验证流结束时未 flush 的缓冲仍被返回。
func TestSSEScanner_FlushesTrailingEvent(t *testing.T) {
	sc := newSSEScanner(strings.NewReader("data: 尾部无空行"))
	var got []string
	for sc.Scan() {
		if d, ok := sc.Data(); ok {
			got = append(got, d)
		}
	}
	if len(got) != 1 || got[0] != "尾部无空行" {
		t.Errorf("应返回 1 个事件，得到 %v", got)
	}
	if sc.Err() != nil {
		t.Errorf("正常结束不应报错: %v", sc.Err())
	}
}

// TestSSEScanner_IgnoresNonDataFields 验证 event/id/retry 字段被忽略。
func TestSSEScanner_IgnoresNonDataFields(t *testing.T) {
	raw := "event: message\nid: 42\nretry: 3000\ndata: 真正的数据\n\n"
	sc := newSSEScanner(strings.NewReader(raw))
	var got []string
	for sc.Scan() {
		if d, ok := sc.Data(); ok {
			got = append(got, d)
		}
	}
	if len(got) != 1 || got[0] != "真正的数据" {
		t.Errorf("应只返回 data 字段，得到 %v", got)
	}
}

// TestSSEScanner_EmptyInput 验证空输入不产生事件也不报错。
func TestSSEScanner_EmptyInput(t *testing.T) {
	for _, raw := range []string{"", "\n\n\n", ": 只有心跳\n\n"} {
		sc := newSSEScanner(strings.NewReader(raw))
		count := 0
		for sc.Scan() {
			if _, ok := sc.Data(); ok {
				count++
			}
		}
		if count != 0 {
			t.Errorf("输入 %q 不应产生事件，得到 %d 个", raw, count)
		}
		if sc.Err() != nil {
			t.Errorf("输入 %q 不应报错: %v", raw, sc.Err())
		}
	}
}

// ---------------------------------------------------------------------------
// 译文清洗
// ---------------------------------------------------------------------------

// TestCleanTranslation 验证剥离模型常见的包裹与前后缀。
//
// 即使系统提示词明确要求「只输出译文」，模型仍经常加戏。
// 不清洗的话用户拿到的译文里会带着「译文：」这类垃圾。
func TestCleanTranslation(t *testing.T) {
	cases := []struct{ in, want string }{
		{"你好", "你好"},
		{"  你好  ", "你好"},
		{`"你好"`, "你好"},
		{"“你好”", "你好"},
		{"「你好」", "你好"},
		{"译文：你好", "你好"},
		{"译文: 你好", "你好"},
		{"翻译：你好", "你好"},
		{"Translation: Hello", "Hello"},
		{"Here is the translation: Hello", "Hello"},
		{"```\n你好\n```", "你好"},
		{"```text\n你好\n```", "你好"},
		{"", ""},
		{"   ", ""},
		// 不应误伤：正文里的引号
		{`他说"你好"然后离开`, `他说"你好"然后离开`},
		// 不应误伤：多个引号片段
		{`"A" 和 "B"`, `"A" 和 "B"`},
		// 不应误伤：只有开头引号
		{`"未闭合的引号`, `"未闭合的引号`},
	}
	for _, c := range cases {
		if got := cleanTranslation(c.in); got != c.want {
			t.Errorf("cleanTranslation(%q)\n  得: %q\n  期望: %q", c.in, got, c.want)
		}
	}
}

// TestCleanTranslation_PreservesStructure 验证清洗不破坏换行与缩进。
//
// 翻译代码注释、配置文件时必须保持结构，否则译文不可用。
func TestCleanTranslation_PreservesStructure(t *testing.T) {
	in := "第一行\n  缩进行\n\n空行后"
	if got := cleanTranslation(in); got != in {
		t.Errorf("清洗破坏了结构:\n  原: %q\n  得: %q", in, got)
	}
}

// TestStripWrappingQuotes 验证引号剥离的边界（只在成对且安全时剥）。
func TestStripWrappingQuotes(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"你好"`, "你好"},
		{"“你好”", "你好"},
		{`"A" 和 "B"`, `"A" 和 "B"`},
		{`"未闭合`, `"未闭合`},
		{`""`, `""`},
		{"单字", "单字"},
		{"", ""},
	}
	for _, c := range cases {
		if got := stripWrappingQuotes(c.in); got != c.want {
			t.Errorf("stripWrappingQuotes(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 请求校验与配置
// ---------------------------------------------------------------------------

// TestValidateRequest 验证非法请求被拒绝。
func TestValidateRequest(t *testing.T) {
	cases := []struct {
		name string
		req  engine.Request
	}{
		{"空文本", engine.Request{Source: "", TargetLang: "zh"}},
		{"纯空白", engine.Request{Source: "   ", TargetLang: "zh"}},
		{"空目标语言", engine.Request{Source: "hi", TargetLang: ""}},
		{"目标语言是 auto", engine.Request{Source: "hi", TargetLang: "auto"}},
		{"未知目标语言", engine.Request{Source: "hi", TargetLang: "克林贡语"}},
		{"未知源语言", engine.Request{Source: "hi", SourceLang: "克林贡语", TargetLang: "zh"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateRequest(c.req)
			if err == nil {
				t.Fatal("应报错")
			}
			var ee *engine.Error
			if !errors.As(err, &ee) {
				t.Fatalf("应返回 *engine.Error，得到 %T", err)
			}
			if ee.Kind != engine.KindConfig {
				t.Errorf("应归类为 KindConfig，得到 %s", ee.KindName())
			}
		})
	}
	valid := []engine.Request{
		{Source: "hi", TargetLang: "zh"},
		{Source: "hi", SourceLang: "auto", TargetLang: "zh"},
		{Source: "hi", SourceLang: "en", TargetLang: "zh-TW"},
	}
	for _, r := range valid {
		if err := validateRequest(r); err != nil {
			t.Errorf("合法请求被拒绝 %+v: %v", r, err)
		}
	}
}

// TestBuild_Validation 验证构造引擎时的配置校验。
func TestBuild_Validation(t *testing.T) {
	if _, err := engine.Build(Kind, engine.Config{"model": "m"}, engine.Secrets{"apiKey": "k"}, engine.Options{}); err == nil {
		t.Error("缺端点应报错")
	}
	if _, err := engine.Build(Kind, engine.Config{"endpoint": "https://a.com"}, engine.Secrets{"apiKey": "k"}, engine.Options{}); err == nil {
		t.Error("缺模型名应报错")
	}
	if _, err := engine.Build(Kind,
		engine.Config{"endpoint": "https://api.example.com", "model": "m"},
		engine.Secrets{}, engine.Options{}); err == nil {
		t.Error("非本地端点缺 API Key 应报错")
	}
	// noAuth=true 时允许非本地端点无 Key —— 这是内置免费引擎唯一的构造路径。
	// 必须有测试钉住：这个开关一旦被改动，内置引擎会在启动播种后第一次翻译时才炸，
	// 而那时用户看到的是「免费翻译服务不可用」，很难联想到是构造校验拦住了。
	if _, err := engine.Build(Kind,
		engine.Config{"endpoint": "https://api.example.com", "model": "m", "noAuth": true},
		engine.Secrets{}, engine.Options{}); err != nil {
		t.Errorf("noAuth=true 应允许无 Key: %v", err)
	}
	// 本地端点可以没有 Key（Ollama 等）
	if _, err := engine.Build(Kind,
		engine.Config{"endpoint": "http://127.0.0.1:11434/v1", "model": "qwen"},
		engine.Secrets{}, engine.Options{}); err != nil {
		t.Errorf("本地端点应允许无 Key: %v", err)
	}
	if _, err := engine.Build(Kind,
		engine.Config{"endpoint": "ftp://a.com", "model": "m"},
		engine.Secrets{"apiKey": "k"}, engine.Options{}); err == nil {
		t.Error("非 http/https 应报错")
	}
	// 未知 kind
	if _, err := engine.Build("不存在的引擎", nil, nil, engine.Options{}); err == nil {
		t.Error("未知 kind 应报错")
	}
}

// TestBuild_ConfigClamping 验证配置值被夹到合法范围。
//
// 服务端不能信任前端传来的值：timeoutSecs=0 会让请求永不超时把 goroutine 挂死。
func TestBuild_ConfigClamping(t *testing.T) {
	cases := []struct {
		cfg     engine.Config
		wantMax int
		why     string
	}{
		// 下限是 100 而不是过去的 500：内置免费引擎的服务端有 30 秒网关硬超时，
		// 单段安全上限只有 400 字符，500 的下限会让它【无法】表达自己需要的分段粒度。
		{engine.Config{"endpoint": "https://a.com", "model": "m", "maxChars": 10}, 100, "低于下限应夹到 100"},
		{engine.Config{"endpoint": "https://a.com", "model": "m", "maxChars": 99999999}, 100000, "高于上限应夹到 100000"},
		{engine.Config{"endpoint": "https://a.com", "model": "m", "maxChars": 5000}, 5000, "范围内应保持不变"},
		{engine.Config{"endpoint": "https://a.com", "model": "m"}, defaultMaxChars, "未设置应用默认值"},
	}
	for _, c := range cases {
		e, err := engine.Build(Kind, c.cfg, engine.Secrets{"apiKey": "k"}, engine.Options{})
		if err != nil {
			t.Fatalf("构造失败: %v", err)
		}
		if got := e.MaxChars(); got != c.wantMax {
			t.Errorf("%s: MaxChars = %d, 期望 %d", c.why, got, c.wantMax)
		}
	}
}

// TestBuild_SecretTrimmed 验证密钥首尾空白被去除。
//
// 用户从网页复制 Key 时常带上换行或空格，不去掉会导致鉴权失败，
// 而错误提示是「Key 无效」，用户完全想不到是多了个空格。
func TestBuild_SecretTrimmed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-clean" {
			t.Errorf("Authorization = %q, 期望 'Bearer sk-clean'", got)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	e, err := engine.Build(Kind,
		engine.Config{"endpoint": srv.URL, "model": "m"},
		engine.Secrets{"apiKey": "  sk-clean\n"},
		engine.Options{})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if _, err := e.Translate(context.Background(), baseRequest("hi")); err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
}

// TestExtraHeaders 验证额外请求头的解析与安全约束。
func TestExtraHeaders(t *testing.T) {
	raw := "X-Tenant-Id: my-org\nX-Custom: value\n# 注释行\n\n非法行没有冒号\n: 空名字\nAuthorization: Bearer 恶意\nContent-Type: text/plain"
	got := parseExtraHeaders(raw)
	if got["X-Tenant-Id"] != "my-org" {
		t.Errorf("X-Tenant-Id = %q", got["X-Tenant-Id"])
	}
	if got["X-Custom"] != "value" {
		t.Errorf("X-Custom = %q", got["X-Custom"])
	}
	if _, ok := got["Authorization"]; ok {
		t.Error("Authorization 不应可通过额外头覆盖（会导致「配了 Key 却用了另一个 Key」）")
	}
	if _, ok := got["Content-Type"]; ok {
		t.Error("Content-Type 不应可覆盖（会让请求体解析失败）")
	}
	if len(got) != 2 {
		t.Errorf("应只解析出 2 个合法头，得到 %d: %v", len(got), got)
	}
	if parseExtraHeaders("") != nil {
		t.Error("空输入应返回 nil")
	}
}

// TestExtraHeaders_Applied 验证额外头确实被发送到上游。
func TestExtraHeaders_Applied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Tenant-Id"); got != "my-org" {
			t.Errorf("X-Tenant-Id = %q, 期望 my-org", got)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL, engine.Config{"extraHeaders": "X-Tenant-Id: my-org"})
	if _, err := e.Translate(context.Background(), baseRequest("hi")); err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
}

// TestCustomPrompt 验证自定义系统提示词生效且占位符被替换。
func TestCustomPrompt(t *testing.T) {
	var gotSystem string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body chatRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotSystem = body.Messages[0].Content
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL, engine.Config{
		"systemPrompt": "把内容翻译成{targetLang}，源语言是{sourceLang}。",
	})
	if _, err := e.Translate(context.Background(), baseRequest("hi")); err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	// {targetLang} 被替换为 lang.LLMName("zh") = "Simplified Chinese"。
	// 用英文自然语言名而非中文名是刻意的：模型对 "Simplified Chinese" 的
	// 理解比 "简体中文" 更稳定（尤其是非中文母语的模型）。
	if !strings.Contains(gotSystem, "Simplified Chinese") {
		t.Errorf("{targetLang} 未被替换为 LLM 语言名: %q", gotSystem)
	}
	if !strings.Contains(gotSystem, "English") {
		t.Errorf("{sourceLang} 未被替换为 LLM 语言名: %q", gotSystem)
	}
	if strings.Contains(gotSystem, "{targetLang}") || strings.Contains(gotSystem, "{sourceLang}") {
		t.Errorf("占位符残留: %q", gotSystem)
	}
}

// TestDefaultPrompt_Content 验证内置提示词包含全部关键约束。
//
// 这些约束每一条都对应一个真实的失败模式，缺一条就会让译文不可用。
func TestDefaultPrompt_Content(t *testing.T) {
	required := []string{
		"{targetLang}",
		"只输出译文",
		"引号",
		"换行",
		"占位符",
		"代码块",
	}
	for _, kw := range required {
		if !strings.Contains(defaultSystemPrompt, kw) {
			t.Errorf("内置提示词缺少关键约束 %q", kw)
		}
	}
	expanded := expandTemplate(defaultSystemPrompt, "简体中文", "英语", "")
	if strings.Contains(expanded, "{targetLang}") {
		t.Error("{targetLang} 未被替换")
	}
	if !strings.Contains(expanded, "简体中文") {
		t.Error("替换后应含目标语言名")
	}
}

// TestExpandTemplate_PercentSafe 验证模板里的 % 不会触发格式化错误。
//
// 用 strings.Replacer 而非 fmt.Sprintf 的原因：用户模板里可能出现任意 %
// （例如翻译含百分号的文本），Sprintf 会报 "unknown verb"。
func TestExpandTemplate_PercentSafe(t *testing.T) {
	tmpl := "翻译到{targetLang}，保留 100% 和 %s %d 这些占位符"
	got := expandTemplate(tmpl, "简体中文", "英语", "正文")
	want := "翻译到简体中文，保留 100% 和 %s %d 这些占位符"
	if got != want {
		t.Errorf("结果 = %q\n期望 = %q", got, want)
	}
	if got2 := expandTemplate("正文是{text}", "zh", "en", "HELLO"); got2 != "正文是HELLO" {
		t.Errorf("{text} 未替换: %q", got2)
	}
}

// ---------------------------------------------------------------------------
// 注册表与接口契约
// ---------------------------------------------------------------------------

// TestRegistration 验证引擎已正确注册且 schema 完整。
func TestRegistration(t *testing.T) {
	found := false
	for _, k := range engine.Kinds() {
		if k == Kind {
			found = true
		}
	}
	if !found {
		t.Fatalf("引擎 %s 未注册，已注册: %v", Kind, engine.Kinds())
	}

	_, sch, ok := engine.Lookup(Kind)
	if !ok {
		t.Fatal("Lookup 失败")
	}
	if sch.Kind != Kind {
		t.Errorf("schema.Kind = %q, 期望 %q", sch.Kind, Kind)
	}
	if !sch.Streaming {
		t.Error("openai 应声明支持流式")
	}
	if sch.MaxChars != defaultMaxChars {
		t.Errorf("schema.MaxChars = %d, 期望 %d", sch.MaxChars, defaultMaxChars)
	}
	if !sch.NeedsSecret {
		t.Error("openai 需要密钥")
	}
	if len(sch.Fields) == 0 {
		t.Error("schema 没有字段定义")
	}
	keys := map[string]bool{}
	for _, f := range sch.Fields {
		if f.Key == "" || f.Label == "" || f.Type == "" {
			t.Errorf("字段定义不完整: %+v", f)
		}
		if (f.Type == engine.FieldSelect || f.Type == engine.FieldRadio) && len(f.Options) == 0 {
			t.Errorf("字段 %s 是 %s 但没有 options", f.Key, f.Type)
		}
		keys[f.Key] = true
	}
	for _, need := range []string{"endpoint", "model", "apiKey"} {
		if !keys[need] {
			t.Errorf("schema 缺少核心字段 %s", need)
		}
	}
	// apiKey 必须标记为 Secret，否则前端不会掩码显示、侧车不会加密
	for _, f := range sch.Fields {
		if f.Key == "apiKey" && !f.Secret {
			t.Error("apiKey 字段必须标记 Secret=true")
		}
		if f.Secret && f.Type != engine.FieldPassword {
			t.Errorf("Secret 字段 %s 的类型应是 password，得到 %s", f.Key, f.Type)
		}
	}
}

// TestInterfaceCompliance 验证实现满足 Engine 接口的全部契约。
func TestInterfaceCompliance(t *testing.T) {
	e := newTestEngine(t, "https://api.example.com/v1", nil)
	if e.Kind() != Kind {
		t.Errorf("Kind() = %q", e.Kind())
	}
	if !e.SupportsStreaming() {
		t.Error("SupportsStreaming() 应为 true")
	}
	if e.MaxChars() <= 0 {
		t.Errorf("MaxChars() = %d, 应为正数", e.MaxChars())
	}
	langs := e.SupportedLangs()
	if len(langs) == 0 {
		t.Error("SupportedLangs() 不应为空")
	}
	want := map[string]bool{"auto": false, "zh": false, "en": false}
	for _, l := range langs {
		if _, ok := want[l]; ok {
			want[l] = true
		}
	}
	for l, ok := range want {
		if !ok {
			t.Errorf("应支持语言 %s", l)
		}
	}
}

// TestMustHostname 验证从 URL 提取主机名（含端口与路径的情况）。
func TestMustHostname(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://api.example.com/v1/chat", "api.example.com"},
		{"http://127.0.0.1:11434/v1", "127.0.0.1"},
		{"https://localhost:8080", "localhost"},
		{"https://a.com?x=1", "a.com"},
		{"https://a.com#frag", "a.com"},
	}
	for _, c := range cases {
		if got := mustHostname(c.in); got != c.want {
			t.Errorf("mustHostname(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// TestConfigFloat 验证浮点配置解析与夹取。
func TestConfigFloat(t *testing.T) {
	cases := []struct {
		cfg  engine.Config
		want float64
		why  string
	}{
		{engine.Config{"temperature": 0.7}, 0.7, "正常值"},
		{engine.Config{"temperature": 5.0}, 2.0, "超上限应夹到 2"},
		{engine.Config{"temperature": -1.0}, 0.0, "低于下限应夹到 0"},
		{engine.Config{"temperature": 1}, 1.0, "int 应被接受"},
		{engine.Config{}, defaultTemperature, "未设置应用默认值"},
		{engine.Config{"temperature": "非法"}, defaultTemperature, "非法类型应用默认值"},
		{nil, defaultTemperature, "nil 配置应用默认值"},
	}
	for _, c := range cases {
		if got := configFloat(c.cfg, "temperature", defaultTemperature, 0, 2); got != c.want {
			t.Errorf("%s: configFloat = %v, 期望 %v", c.why, got, c.want)
		}
	}
}
