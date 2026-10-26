package tencent

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vyrnax/translation/engine"
)

const (
	testSecretID  = "AKIDzTestSecretIdXXXXXXXXXXXXX"
	testSecretKey = "TestSecretKey1234567890abcdef"
)

func newTestEngine(t *testing.T, endpoint string) engine.Engine {
	t.Helper()
	e, err := engine.Build(Kind,
		engine.Config{"secretId": testSecretID, "endpoint": endpoint},
		engine.Secrets{"secretKey": testSecretKey},
		engine.Options{Name: "测试腾讯云"})
	if err != nil {
		t.Fatalf("构造引擎失败: %v", err)
	}
	return e
}

func req() engine.Request {
	return engine.Request{Source: "hello world", SourceLang: "en", TargetLang: "zh"}
}

// TestTC3_ThreeLayerDerivation 验证 TC3 的三层 HMAC 密钥派生。
//
// 这是腾讯云签名最容易写错的地方：
//
//	SecretDate    = HMAC-SHA256("TC3"+SecretKey, Date)
//	SecretService = HMAC-SHA256(SecretDate, "tmt")
//	SecretSigning = HMAC-SHA256(SecretService, "tc3_request")
//	Signature     = Hex(HMAC-SHA256(SecretSigning, StringToSign))
//
// 注意第一层的 key 是 "TC3" 前缀拼接 SecretKey，不是 SecretKey 本身。
// 漏掉 "TC3" 前缀是最常见的错误，症状是 AuthFailure.SignatureFailure。
func TestTC3_ThreeLayerDerivation(t *testing.T) {
	date := "2026-01-15"
	stringToSign := "TC3-HMAC-SHA256\n1700000000\n2026-01-15/tmt/tc3_request\nabc123"

	// 独立按规范算一遍
	secretDate := hmacRaw([]byte("TC3"+testSecretKey), date)
	secretService := hmacRaw(secretDate, service)
	secretSigning := hmacRaw(secretService, "tc3_request")
	want := hex.EncodeToString(hmacRaw(secretSigning, stringToSign))

	// 用包内实现算
	gotDate := hmacSHA256Raw([]byte("TC3"+testSecretKey), date)
	gotService := hmacSHA256Raw(gotDate, service)
	gotSigning := hmacSHA256Raw(gotService, "tc3_request")
	got := hex.EncodeToString(hmacSHA256Raw(gotSigning, stringToSign))

	if got != want {
		t.Errorf("三层派生结果不匹配\n  得: %s\n  期望: %s", got, want)
	}
	if len(got) != 64 {
		t.Errorf("签名长度 = %d, 期望 64（SHA256 十六进制）", len(got))
	}

	// 反向确认：漏掉 "TC3" 前缀会得到不同结果（证明前缀确实生效）
	wrong := hex.EncodeToString(hmacSHA256Raw(
		hmacSHA256Raw(hmacSHA256Raw([]byte(testSecretKey), date), service), stringToSign))
	if wrong == got {
		t.Error("去掉 TC3 前缀后签名竟然相同，说明前缀未参与派生")
	}
}

func hmacRaw(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

// TestTranslate_AuthorizationHeaderFormat 验证 Authorization 头格式。
//
// 必须是：TC3-HMAC-SHA256 Credential=<SecretId>/<date>/tmt/tc3_request,
//
//	SignedHeaders=content-type;host, Signature=<hex>
//
// 注意腾讯云用的是「逗号 + 空格」分隔（与阿里云的「逗号无空格」不同），
// 这个差异很容易搞混。
func TestTranslate_AuthorizationHeaderFormat(t *testing.T) {
	var gotAuth, gotAction, gotVersion, gotTS, gotRegion, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAction = r.Header.Get("X-TC-Action")
		gotVersion = r.Header.Get("X-TC-Version")
		gotTS = r.Header.Get("X-TC-Timestamp")
		gotRegion = r.Header.Get("X-TC-Region")
		gotCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Response":{"TargetText":"你好世界","Source":"en","Target":"zh","RequestId":"r1"}}`))
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

	if !strings.HasPrefix(gotAuth, "TC3-HMAC-SHA256 Credential="+testSecretID+"/") {
		t.Errorf("Authorization 前缀错误: %q", gotAuth)
	}
	if !strings.Contains(gotAuth, "/tmt/tc3_request,") {
		t.Errorf("Authorization 缺少 credentialScope: %q", gotAuth)
	}
	if !strings.Contains(gotAuth, "SignedHeaders=content-type;host") {
		t.Errorf("SignedHeaders 应为 content-type;host: %q", gotAuth)
	}
	if !strings.Contains(gotAuth, ", Signature=") {
		t.Errorf("腾讯云用「逗号+空格」分隔，与阿里云不同: %q", gotAuth)
	}
	sig := gotAuth[strings.Index(gotAuth, ", Signature=")+len(", Signature="):]
	if len(sig) != 64 {
		t.Errorf("Signature 长度 = %d, 期望 64", len(sig))
	}

	if gotAction != "TextTranslate" {
		t.Errorf("X-TC-Action = %q, 期望 TextTranslate", gotAction)
	}
	if gotVersion != "2018-03-21" {
		t.Errorf("X-TC-Version = %q", gotVersion)
	}
	// X-TC-Timestamp 必须是【秒级】unix 时间戳（10 位）
	ts, err := strconv.ParseInt(gotTS, 10, 64)
	if err != nil {
		t.Fatalf("X-TC-Timestamp 不是整数: %q", gotTS)
	}
	if len(gotTS) != 10 {
		t.Errorf("X-TC-Timestamp 长度 = %d, 期望 10（秒级；毫秒级会导致签名失败）", len(gotTS))
	}
	// 时间戳与实际时间偏差必须在 5 分钟内（超过会签名失败）
	if diff := time.Now().Unix() - ts; diff > 300 || diff < -300 {
		t.Errorf("X-TC-Timestamp 与当前时间偏差 %d 秒，超过 5 分钟会签名失败", diff)
	}
	if gotRegion == "" {
		t.Error("X-TC-Region 不应为空")
	}
	if gotCT != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q（必须与参与签名的值完全一致）", gotCT)
	}
}

// TestTranslate_SignatureVerifiable 独立重算签名并比对。
func TestTranslate_SignatureVerifiable(t *testing.T) {
	var (
		gotAuth, gotTS, gotCT string
		gotHost               string
		gotBody               []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotTS = r.Header.Get("X-TC-Timestamp")
		gotCT = r.Header.Get("Content-Type")
		gotHost = r.Host
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"Response":{"TargetText":"ok","RequestId":"r1"}}`))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL)
	if _, err := e.Translate(context.Background(), req()); err != nil {
		t.Fatalf("翻译失败: %v", err)
	}

	// 从 Authorization 头里取出 credentialScope 中的 date
	credPart := gotAuth[len("TC3-HMAC-SHA256 Credential="):]
	credPart = credPart[:strings.Index(credPart, ",")]
	date := credPart[strings.Index(credPart, "/")+1 : strings.Index(credPart, "/tmt/")]

	payloadHash := sha256Hex(gotBody)
	canonicalRequest := "POST\n/\n\n" +
		"content-type:" + gotCT + "\n" +
		"host:" + gotHost + "\n" + "\n" +
		"content-type;host" + "\n" +
		payloadHash
	stringToSign := "TC3-HMAC-SHA256\n" + gotTS + "\n" +
		date + "/tmt/tc3_request\n" + sha256Hex([]byte(canonicalRequest))

	secretDate := hmacRaw([]byte("TC3"+testSecretKey), date)
	secretService := hmacRaw(secretDate, "tmt")
	secretSigning := hmacRaw(secretService, "tc3_request")
	want := hex.EncodeToString(hmacRaw(secretSigning, stringToSign))

	got := gotAuth[strings.Index(gotAuth, ", Signature=")+len(", Signature="):]
	if got != want {
		t.Errorf("签名不匹配\n  得: %s\n  期望: %s\n  date=%s ts=%s", got, want, date, gotTS)
	}
}

// TestTranslate_RequestBody 验证请求体字段。
func TestTranslate_RequestBody(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"Response":{"TargetText":"ok","RequestId":"r1"}}`))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL)
	if _, err := e.Translate(context.Background(), req()); err != nil {
		t.Fatalf("失败: %v", err)
	}
	if body["SourceText"] != "hello world" {
		t.Errorf("SourceText = %v", body["SourceText"])
	}
	if body["Source"] != "en" {
		t.Errorf("Source = %v, 期望 en", body["Source"])
	}
	if body["Target"] != "zh" {
		t.Errorf("Target = %v, 期望 zh", body["Target"])
	}
	if _, ok := body["ProjectId"]; !ok {
		t.Error("缺少 ProjectId（腾讯云必填，默认 0）")
	}
	// 密钥绝不能出现在请求体里
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), testSecretKey) {
		t.Error("请求体泄露了 SecretKey")
	}
}

// TestTranslate_AutoSource 验证 auto 源语言被正确传递。
func TestTranslate_AutoSource(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"Response":{"TargetText":"ok","RequestId":"r1"}}`))
	}))
	defer srv.Close()
	e := newTestEngine(t, srv.URL)
	if _, err := e.Translate(context.Background(), engine.Request{
		Source: "hi", SourceLang: "auto", TargetLang: "zh",
	}); err != nil {
		t.Fatalf("失败: %v", err)
	}
	if body["Source"] != "auto" {
		t.Errorf("Source = %v, 期望 auto", body["Source"])
	}
}

// TestTranslate_ErrorCodes 验证业务错误码映射。
func TestTranslate_ErrorCodes(t *testing.T) {
	cases := []struct {
		code string
		kind engine.ErrorKind
	}{
		{"AuthFailure.SignatureFailure", engine.KindAuth},
		{"AuthFailure.SecretIdNotFound", engine.KindAuth},
		{"AuthFailure.UnauthorizedOperation", engine.KindAuth},
		{"RequestLimitExceeded", engine.KindRateLimit},
		{"ResourceUnavailable.InsufficientBalance", engine.KindRateLimit},
		{"InvalidParameter", engine.KindConfig},
		{"UnsupportedOperation.LanguageNotSupport", engine.KindUnsupported},
		{"InternalError", engine.KindUpstream},
		{"SomethingWeird", engine.KindUpstream},
	}
	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			resp := `{"Response":{"Error":{"Code":"` + c.code + `","Message":"测试错误"},"RequestId":"r1"}}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(resp))
			}))
			defer srv.Close()
			e := newTestEngine(t, srv.URL)
			_, err := e.Translate(context.Background(), req())
			if err == nil {
				t.Fatalf("Code=%s 应报错", c.code)
			}
			var ee *engine.Error
			if !asEngineErr(err, &ee) {
				t.Fatalf("应返回 *engine.Error，得到 %T", err)
			}
			if ee.Kind != c.kind {
				t.Errorf("Code=%s 应归类为 %s，得到 %s（%s）",
					c.code, kindName(c.kind), ee.KindName(), ee.Message)
			}
		})
	}
}

// TestTranslate_TooLong 验证超长文本在本地被拦截。
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

// TestBuild_Validation 验证构造期校验与默认端点。
func TestBuild_Validation(t *testing.T) {
	if _, err := engine.Build(Kind, engine.Config{}, engine.Secrets{"secretKey": "s"}, engine.Options{}); err == nil {
		t.Error("缺 secretId 应报错")
	}
	if _, err := engine.Build(Kind, engine.Config{"secretId": "i"}, engine.Secrets{}, engine.Options{}); err == nil {
		t.Error("缺 secretKey 应报错")
	}
	e, err := engine.Build(Kind, engine.Config{"secretId": "i"}, engine.Secrets{"secretKey": "s"}, engine.Options{})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if e.(*engineImpl).endpoint != defaultEndpoint {
		t.Errorf("默认 endpoint = %q", e.(*engineImpl).endpoint)
	}
	if e.SupportsStreaming() {
		t.Error("腾讯云不支持流式")
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
	keys := map[string]bool{}
	for _, f := range sch.Fields {
		keys[f.Key] = true
		if (f.Type == engine.FieldSelect || f.Type == engine.FieldRadio) && len(f.Options) == 0 {
			t.Errorf("字段 %s 是 %s 但没有 options", f.Key, f.Type)
		}
		if f.Secret && f.Type != engine.FieldPassword {
			t.Errorf("Secret 字段 %s 类型应为 password", f.Key)
		}
	}
	for _, need := range []string{"secretId", "secretKey", "region"} {
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
