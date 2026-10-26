package aliyun

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vyrnax/translation/engine"
)

const (
	testAKID     = "LTAI5tTestAccessKeyId"
	testAKSecret = "TestAccessKeySecret1234567890"
)

func newTestEngine(t *testing.T, endpoint string) engine.Engine {
	t.Helper()
	e, err := engine.Build(Kind,
		engine.Config{"accessKeyId": testAKID, "endpoint": endpoint},
		engine.Secrets{"accessKeySecret": testAKSecret},
		engine.Options{Name: "测试阿里云"})
	if err != nil {
		t.Fatalf("构造引擎失败: %v", err)
	}
	return e
}

func req() engine.Request {
	return engine.Request{Source: "hello world", SourceLang: "en", TargetLang: "zh"}
}

// TestPercentEncode_SpaceIsNotPlus 是 ACS3 签名正确性的【最关键】测试。
//
// 不能用 url.QueryEscape：它把空格编码成 "+"，而 RFC3986/ACS3 要求 "%20"。
// 翻译文本里空格极常见（英文句子、代码片段），一旦用错编码，
// 所有含空格的请求都会 SignatureDoesNotMatch —— 而不含空格的却能成功，
// 这种「有时能翻有时不能」的症状极难定位到编码差异上。
func TestPercentEncode_SpaceIsNotPlus(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hello world", "hello%20world"}, // 空格 → %20，绝不是 +
		{"a+b", "a%2Bb"},                 // 加号本身要编码
		{"100%", "100%25"},               // 百分号要编码
		{"a=b&c=d", "a%3Db%26c%3Dd"},     // 等号与与号要编码
		// RFC3986 的 unreserved 集合是 A-Z a-z 0-9 - _ . ~，这六个都【不】编码。
		// 注意 ~ 不编码是 ACS3(V3) 的要求 —— 老版 RPC 签名(V1)会先编码再把 %7E
		// 换回 ~，结果相同，但 V3 直接不编码。
		{"~-_.", "~-_."},
		{"/", "%2F"},                 // 斜杠必须编码（否则破坏 query 结构）
		{"*", "%2A"},                 // 星号必须编码（url.QueryEscape 不编码它）
		{"中文", "%E4%B8%AD%E6%96%87"}, // UTF-8 多字节逐字节编码，大写十六进制
		{"abcXYZ019", "abcXYZ019"},   // 字母数字不编码
		{"", ""},
	}
	for _, c := range cases {
		if got := percentEncode(c.in); got != c.want {
			t.Errorf("percentEncode(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
	// 反向确认：结果里绝不能出现 "+"（那说明用了 QueryEscape 的语义）
	if got := percentEncode("a b"); strings.Contains(got, "+") {
		t.Errorf("编码结果含加号，说明用了 url.QueryEscape 语义: %q", got)
	}
	// 十六进制必须大写（ACS3 要求）
	if got := percentEncode("中"); got != strings.ToUpper(got) {
		t.Errorf("百分号编码应为大写十六进制: %q", got)
	}
}

// TestCanonicalQueryString_Sorted 验证参数按名字典序排序。
//
// 不排序的话 map 遍历顺序随机，每次请求的签名都不同 —— 症状是
// 「同样的配置有时成功有时失败」，这是最难排查的一类 bug。
func TestCanonicalQueryString_Sorted(t *testing.T) {
	params := map[string]string{
		"Zebra":   "z",
		"Action":  "TranslateGeneral",
		"apple":   "a",
		"Version": "2018-10-12",
	}
	got := canonicalQueryString(params)
	// 字典序（大写在小写前）：Action, Version, Zebra, apple
	want := "Action=TranslateGeneral&Version=2018-10-12&Zebra=z&apple=a"
	if got != want {
		t.Errorf("canonicalQueryString\n  得: %s\n  期望: %s", got, want)
	}
	// 多次调用结果必须完全一致（证明没有 map 遍历随机性）
	for i := 0; i < 20; i++ {
		if again := canonicalQueryString(params); again != got {
			t.Fatalf("第 %d 次调用结果不同，说明排序不稳定:\n  %s\n  %s", i, got, again)
		}
	}
}

// TestCanonicalQueryString_EncodesValues 验证值也被编码（不只是键）。
func TestCanonicalQueryString_EncodesValues(t *testing.T) {
	got := canonicalQueryString(map[string]string{"SourceText": "hello world & more"})
	want := "SourceText=hello%20world%20%26%20more"
	if got != want {
		t.Errorf("值未正确编码\n  得: %s\n  期望: %s", got, want)
	}
}

// TestSortedHeaderNames 验证头名小写且排序。
func TestSortedHeaderNames(t *testing.T) {
	got := sortedHeaderNames(map[string]string{
		"X-Acs-Version": "v",
		"Content-Type":  "ct",
		"Host":          "h",
		"x-acs-date":    "d",
	})
	want := []string{"content-type", "host", "x-acs-date", "x-acs-version"}
	if strings.Join(got, ";") != strings.Join(want, ";") {
		t.Errorf("sortedHeaderNames = %v, 期望 %v", got, want)
	}
}

// TestBuildCanonicalRequest_Structure 验证规范请求的六段结构。
//
// ACS3 的规范请求必须严格按 METHOD / URI / QueryString / Headers /
// SignedHeaders / PayloadHash 六段用 \n 分隔，段数或顺序错都会签名失败。
func TestBuildCanonicalRequest_Structure(t *testing.T) {
	headers := map[string]string{
		"content-type": "application/x-www-form-urlencoded",
		"host":         "mt.cn-hangzhou.aliyuncs.com",
		"x-acs-date":   "2026-01-01T00:00:00Z",
	}
	params := map[string]string{"Action": "TranslateGeneral", "Version": "2018-10-12"}
	payloadHash := sha256Hex([]byte{})

	got := buildCanonicalRequest("POST", "/", params, headers, payloadHash)
	lines := strings.Split(got, "\n")

	// 结构：METHOD(1) + URI(1) + QS(1) + Headers(3行,每行以\n结尾→产生4段) + SignedHeaders(1) + Hash(1)
	if lines[0] != "POST" {
		t.Errorf("第 1 段应为 METHOD，得到 %q", lines[0])
	}
	if lines[1] != "/" {
		t.Errorf("第 2 段应为 CanonicalURI，得到 %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "Action=TranslateGeneral") {
		t.Errorf("第 3 段应为 CanonicalQueryString，得到 %q", lines[2])
	}
	// 头段：每行 name:value
	if !strings.HasPrefix(lines[3], "content-type:") {
		t.Errorf("第 4 段起应为规范头，得到 %q", lines[3])
	}
	// 最后一段是 payload hash
	if lines[len(lines)-1] != payloadHash {
		t.Errorf("末段应为 PayloadHash，得到 %q", lines[len(lines)-1])
	}
	// 倒数第二段是 SignedHeaders（分号连接）
	signed := lines[len(lines)-2]
	if !strings.Contains(signed, ";") || !strings.Contains(signed, "host") {
		t.Errorf("倒数第 2 段应为 SignedHeaders，得到 %q", signed)
	}
}

// TestTranslate_AuthorizationHeaderFormat 验证 Authorization 头的格式。
//
// 必须是：ACS3-HMAC-SHA256 Credential=<AK>,SignedHeaders=<...>,Signature=<hex>
// 三段用逗号连接且【无空格】，格式错了阿里云返回 InvalidAuthorization。
func TestTranslate_AuthorizationHeaderFormat(t *testing.T) {
	var gotAuth, gotAction, gotVersion, gotDate, gotNonce, gotHash string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAction = r.Header.Get("x-acs-action")
		gotVersion = r.Header.Get("x-acs-version")
		gotDate = r.Header.Get("x-acs-date")
		gotNonce = r.Header.Get("x-acs-signature-nonce")
		gotHash = r.Header.Get("x-acs-content-sha256")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Code":"200","Data":{"Translated":"你好世界"},"RequestId":"test-req-id"}`))
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

	if !strings.HasPrefix(gotAuth, "ACS3-HMAC-SHA256 Credential="+testAKID+",") {
		t.Errorf("Authorization 前缀错误: %q", gotAuth)
	}
	if !strings.Contains(gotAuth, ",SignedHeaders=") {
		t.Errorf("Authorization 缺少 SignedHeaders: %q", gotAuth)
	}
	if !strings.Contains(gotAuth, ",Signature=") {
		t.Errorf("Authorization 缺少 Signature: %q", gotAuth)
	}
	// 逗号后不能有空格
	if strings.Contains(gotAuth, ", ") {
		t.Errorf("Authorization 的逗号后不应有空格: %q", gotAuth)
	}
	// Signature 必须是 64 位十六进制（SHA256）
	sig := gotAuth[strings.Index(gotAuth, ",Signature=")+len(",Signature="):]
	if len(sig) != 64 {
		t.Errorf("Signature 长度 = %d, 期望 64（SHA256 十六进制）", len(sig))
	}
	if sig != strings.ToLower(sig) {
		t.Errorf("Signature 应为小写十六进制: %q", sig)
	}

	if gotAction != "TranslateGeneral" {
		t.Errorf("x-acs-action = %q", gotAction)
	}
	if gotVersion != "2018-10-12" {
		t.Errorf("x-acs-version = %q", gotVersion)
	}
	// x-acs-date 必须是 ISO8601 UTC 且以 Z 结尾
	if !strings.HasSuffix(gotDate, "Z") || len(gotDate) != 20 {
		t.Errorf("x-acs-date 格式错误（应为 2006-01-02T15:04:05Z）: %q", gotDate)
	}
	if gotNonce == "" {
		t.Error("x-acs-signature-nonce 不应为空（防重放）")
	}
	// 空 payload 的 SHA256 是固定值
	wantEmptyHash := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if gotHash != wantEmptyHash {
		t.Errorf("x-acs-content-sha256 = %q, 期望空 payload 的 SHA256 %q", gotHash, wantEmptyHash)
	}
}

// TestTranslate_SignatureVerifiable 独立重算签名并比对。
//
// 这是签名正确性的最终证明：测试自己按 ACS3 规范算一遍，
// 与适配器发出的 Authorization 头比对。
func TestTranslate_SignatureVerifiable(t *testing.T) {
	var (
		gotAuth, gotDate, gotNonce, gotHash, gotAction, gotVersion, gotCT, gotHost string
		gotQuery                                                                   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotDate = r.Header.Get("x-acs-date")
		gotNonce = r.Header.Get("x-acs-signature-nonce")
		gotHash = r.Header.Get("x-acs-content-sha256")
		gotAction = r.Header.Get("x-acs-action")
		gotVersion = r.Header.Get("x-acs-version")
		gotCT = r.Header.Get("Content-Type")
		gotHost = r.Host
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"Code":"200","Data":{"Translated":"ok"}}`))
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL)
	if _, err := e.Translate(context.Background(), req()); err != nil {
		t.Fatalf("翻译失败: %v", err)
	}

	// 用捕获到的实际参数独立重算
	params := map[string]string{}
	for _, kv := range strings.Split(gotQuery, "&") {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			continue
		}
		k, _ := unescape(kv[:i])
		v, _ := unescape(kv[i+1:])
		params[k] = v
	}
	headers := map[string]string{
		"content-type":          gotCT,
		"host":                  gotHost,
		"x-acs-action":          gotAction,
		"x-acs-content-sha256":  gotHash,
		"x-acs-date":            gotDate,
		"x-acs-signature-nonce": gotNonce,
		"x-acs-version":         gotVersion,
	}
	canonicalRequest := buildCanonicalRequest("POST", "/", params, headers, gotHash)
	stringToSign := "ACS3-HMAC-SHA256\n" + sha256Hex([]byte(canonicalRequest))
	mac := hmac.New(sha256.New, []byte(testAKSecret))
	mac.Write([]byte(stringToSign))
	wantSig := hex.EncodeToString(mac.Sum(nil))

	gotSig := gotAuth[strings.Index(gotAuth, ",Signature=")+len(",Signature="):]
	if gotSig != wantSig {
		t.Errorf("签名不匹配\n  得: %s\n  期望: %s", gotSig, wantSig)
	}
}

// unescape 是测试用的百分号解码（避免引入 net/url 的 QueryUnescape 语义差异）。
func unescape(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			var v byte
			if _, err := hex.Decode([]byte{0}, []byte{}); err == nil {
				_ = v
			}
			hi, e1 := hexVal(s[i+1])
			lo, e2 := hexVal(s[i+2])
			if e1 == nil && e2 == nil {
				b.WriteByte(hi<<4 | lo)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String(), nil
}

func hexVal(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, nil
	}
	return 0, errInvalidHex
}

var errInvalidHex = &hexError{}

type hexError struct{}

func (h *hexError) Error() string { return "非法十六进制字符" }

// TestTranslate_AutoSource 验证 auto 映射为阿里的 "auto"。
func TestTranslate_AutoSource(t *testing.T) {
	var gotSrc string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSrc = r.URL.Query().Get("SourceLanguage")
		_, _ = w.Write([]byte(`{"Code":"200","Data":{"Translated":"ok"}}`))
	}))
	defer srv.Close()
	e := newTestEngine(t, srv.URL)
	if _, err := e.Translate(context.Background(), engine.Request{
		Source: "hi", SourceLang: "auto", TargetLang: "zh",
	}); err != nil {
		t.Fatalf("失败: %v", err)
	}
	if gotSrc != "auto" {
		t.Errorf("SourceLanguage = %q, 期望 auto", gotSrc)
	}
}

// TestTranslate_BusinessErrorCode 验证 HTTP 200 + Code 字段的业务错误处理。
func TestTranslate_BusinessErrorCode(t *testing.T) {
	cases := []struct {
		code string
		kind engine.ErrorKind
	}{
		{"SignatureDoesNotMatch", engine.KindAuth},
		{"InvalidAccessKeyId.NotFound", engine.KindAuth},
		{"Forbidden.RAM", engine.KindAuth},
		{"Throttling.User", engine.KindRateLimit},
		{"InvalidParameter", engine.KindConfig},
		{"UnsupportedLanguage", engine.KindUnsupported},
		{"ServiceUnavailable", engine.KindUpstream},
		{"SomethingWeird", engine.KindUpstream},
	}
	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"Code":"` + c.code + `","Message":"测试错误","RequestId":"r1"}`))
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

// TestBuild_EndpointFromRegion 验证未填 endpoint 时按地域自动生成。
func TestBuild_EndpointFromRegion(t *testing.T) {
	e, err := engine.Build(Kind,
		engine.Config{"accessKeyId": "ak", "region": "cn-shanghai"},
		engine.Secrets{"accessKeySecret": "sk"}, engine.Options{})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	impl, ok := e.(*engineImpl)
	if !ok {
		t.Fatalf("类型断言失败: %T", e)
	}
	if impl.endpoint != "https://mt.cn-shanghai.aliyuncs.com" {
		t.Errorf("endpoint = %q, 期望按地域生成", impl.endpoint)
	}
	// 默认地域
	e2, _ := engine.Build(Kind, engine.Config{"accessKeyId": "ak"},
		engine.Secrets{"accessKeySecret": "sk"}, engine.Options{})
	if e2.(*engineImpl).endpoint != "https://mt.cn-hangzhou.aliyuncs.com" {
		t.Errorf("默认地域 endpoint = %q", e2.(*engineImpl).endpoint)
	}
}

// TestBuild_Validation 验证构造期校验。
func TestBuild_Validation(t *testing.T) {
	if _, err := engine.Build(Kind, engine.Config{}, engine.Secrets{"accessKeySecret": "s"}, engine.Options{}); err == nil {
		t.Error("缺 accessKeyId 应报错")
	}
	if _, err := engine.Build(Kind, engine.Config{"accessKeyId": "a"}, engine.Secrets{}, engine.Options{}); err == nil {
		t.Error("缺 accessKeySecret 应报错")
	}
	e, err := engine.Build(Kind, engine.Config{"accessKeyId": "a"}, engine.Secrets{"accessKeySecret": "s"}, engine.Options{})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if e.SupportsStreaming() {
		t.Error("阿里云不支持流式")
	}
	if len(e.SupportedLangs()) == 0 {
		t.Error("SupportedLangs 为空")
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
	}
	for _, need := range []string{"accessKeyId", "accessKeySecret", "region"} {
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
