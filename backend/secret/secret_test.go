package secret

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
)

const testPluginID = "com.vyrnax.translation"

// 测试用的假密钥。所有断言都围绕「它绝不能出现在密文/日志/错误里」。
const testSecret = "sk-test-ABCDEFGHIJKLMNOP1234567890"

// TestHKDF_KnownVector 用 RFC 5869 附录 A.1 的官方测试向量验证实现正确性。
//
// 这是 HKDF 实现正确性的唯一可靠证明 —— 自己造的往返测试只能证明
// 「加密解密自洽」，证明不了「符合标准」。
func TestHKDF_KnownVector(t *testing.T) {
	// RFC 5869 A.1: SHA-256, IKM=0x0b*22, salt=0x000102...0c, info=0xf0f1...f9, L=42
	ikm := bytes.Repeat([]byte{0x0b}, 22)
	salt := []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
		0x08, 0x09, 0x0a, 0x0b, 0x0c}
	info := []byte{0xf0, 0xf1, 0xf2, 0xf3, 0xf4, 0xf5, 0xf6, 0xf7, 0xf8, 0xf9}

	wantPRK := "077709362c2e32df0ddc3f0dc47bba63" +
		"90b6c73bb50f9c3122ec844ad7c2b3e5"
	wantOKM := "3cb25f25faacd57a90434f64d0362f2a" +
		"2d2d0a90cf1a5a4c5db02d56ecc4c5bf" +
		"34007208d5b887185865"

	prk := hkdfExtract(salt, ikm)
	if got := hexEncode(prk); got != wantPRK {
		t.Errorf("HKDF-Extract 不匹配 RFC 5869 A.1:\n  得: %s\n  期望: %s", got, wantPRK)
	}

	okm, err := hkdfExpand(prk, info, 42)
	if err != nil {
		t.Fatalf("Expand 失败: %v", err)
	}
	if got := hexEncode(okm); got != wantOKM {
		t.Errorf("HKDF-Expand 不匹配 RFC 5869 A.1:\n  得: %s\n  期望: %s", got, wantOKM)
	}
}

func hexEncode(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0x0f])
	}
	return string(out)
}

// TestHKDF_LengthValidation 验证非法长度被拒绝。
func TestHKDF_LengthValidation(t *testing.T) {
	prk := hkdfExtract([]byte("salt"), []byte("ikm"))
	for _, bad := range []int{0, -1, 255*32 + 1} {
		if _, err := hkdfExpand(prk, []byte("info"), bad); err == nil {
			t.Errorf("长度 %d 应被拒绝", bad)
		}
	}
	// 合法边界：255*HashLen
	if _, err := hkdfExpand(prk, []byte("info"), 255*32); err != nil {
		t.Errorf("长度 255*32 应合法，得到 %v", err)
	}
}

// TestHKDF_Deterministic 验证相同输入派生相同密钥（重启后必须能解密旧数据）。
func TestHKDF_Deterministic(t *testing.T) {
	a, _ := hkdfSHA256([]byte("secret"), []byte("salt"), []byte("info"), 32)
	b, _ := hkdfSHA256([]byte("secret"), []byte("salt"), []byte("info"), 32)
	if !bytes.Equal(a, b) {
		t.Error("相同输入应派生相同密钥，否则重启后无法解密已保存的数据")
	}
	// 不同 info 应派生不同密钥（密钥分离）
	c, _ := hkdfSHA256([]byte("secret"), []byte("salt"), []byte("other"), 32)
	if bytes.Equal(a, c) {
		t.Error("不同 info 应派生不同密钥（密钥分离失效）")
	}
}

// TestMachineBound_RoundTrip 验证降级方案的加解密往返。
func TestMachineBound_RoundTrip(t *testing.T) {
	p := newMachineBound(testPluginID)
	if p.Scheme() != SchemeAESGCMMachine {
		t.Errorf("Scheme = %q, 期望 %q", p.Scheme(), SchemeAESGCMMachine)
	}
	// 降级方案必须诚实报告自己不是真加密
	if p.Secure() {
		t.Error("机器绑定方案只是混淆，Secure() 必须返回 false，否则前端不会显示警示条")
	}

	cases := [][]byte{
		[]byte(testSecret),
		[]byte("短"),
		[]byte(strings.Repeat("长密钥", 100)),
		[]byte("含特殊字符 !@#$%^&*()_+-=[]{}|;:,.<>?"),
		[]byte("中文密钥测试"),
	}
	for _, plain := range cases {
		ct, err := p.Protect(plain)
		if err != nil {
			t.Fatalf("加密失败: %v", err)
		}
		got, err := p.Unprotect(ct)
		if err != nil {
			t.Fatalf("解密失败: %v", err)
		}
		if !bytes.Equal(got, plain) {
			t.Errorf("往返不一致:\n  原: %q\n  得: %q", plain, got)
		}
	}
}

// TestMachineBound_CiphertextHidesPlaintext 是最关键的安全断言：
// 密文里绝不能出现明文子串。
func TestMachineBound_CiphertextHidesPlaintext(t *testing.T) {
	p := newMachineBound(testPluginID)
	ct, err := p.Protect([]byte(testSecret))
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	if strings.Contains(ct, testSecret) {
		t.Errorf("密文含明文密钥！\n密文: %s", ct)
	}
	// 明文的可识别片段也不能出现
	for _, frag := range []string{"sk-test", "ABCDEFGHIJ", "1234567890"} {
		if strings.Contains(ct, frag) {
			t.Errorf("密文含明文片段 %q！\n密文: %s", frag, ct)
		}
	}
}

// TestMachineBound_NonceUnique 验证每次加密的 nonce 不同（密文不重复）。
//
// GCM 的 nonce 绝不能重复使用 —— 重复会彻底破坏机密性与完整性。
// 如果两次加密同一明文得到相同密文，说明 nonce 是固定的，这是严重缺陷。
func TestMachineBound_NonceUnique(t *testing.T) {
	p := newMachineBound(testPluginID)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		ct, err := p.Protect([]byte(testSecret))
		if err != nil {
			t.Fatalf("第 %d 次加密失败: %v", i, err)
		}
		if seen[ct] {
			t.Fatalf("第 %d 次加密产出了重复密文，说明 nonce 未随机化", i)
		}
		seen[ct] = true
	}
}

// TestMachineBound_TamperDetected 验证密文被篡改时 GCM 校验失败。
// 绝不能返回部分数据 —— 那等于让攻击者能逐字节爆破。
func TestMachineBound_TamperDetected(t *testing.T) {
	p := newMachineBound(testPluginID)
	ct, err := p.Protect([]byte(testSecret))
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}

	// 篡改 base64 中间的一个字符
	runes := []rune(ct)
	mid := len(runes) / 2
	if runes[mid] == 'A' {
		runes[mid] = 'B'
	} else {
		runes[mid] = 'A'
	}
	tampered := string(runes)

	_, err = p.Unprotect(tampered)
	if err == nil {
		t.Error("篡改后的密文应解密失败")
	}
	if !IsDecryptError(err) {
		t.Errorf("应返回 ErrDecrypt（供上层映射为 -32035），得到 %v", err)
	}
}

// TestMachineBound_InvalidInputs 验证各类非法输入都被拒绝且不 panic。
func TestMachineBound_InvalidInputs(t *testing.T) {
	p := newMachineBound(testPluginID)
	cases := map[string]string{
		"空串":      "",
		"非base64": "这不是base64!!!",
		"过短":      "YWJj", // "abc" 的 base64，长度不足 nonce+tag
		"空白":      "   ",
	}
	for name, in := range cases {
		_, err := p.Unprotect(in)
		if err == nil {
			t.Errorf("%s: 应返回错误", name)
			continue
		}
		if !IsDecryptError(err) {
			t.Errorf("%s: 应返回 ErrDecrypt，得到 %v", name, err)
		}
	}
	// 空明文加密应被拒绝（保存空密钥没有意义，且会掩盖配置错误）
	if _, err := p.Protect([]byte("")); err == nil {
		t.Error("加密空明文应被拒绝")
	}
}

// TestMachineBound_DifferentPluginID 验证 pluginID 参与密钥派生（密钥分离）。
func TestMachineBound_DifferentPluginID(t *testing.T) {
	a := newMachineBound("com.a.plugin")
	b := newMachineBound("com.b.plugin")
	ct, err := a.Protect([]byte(testSecret))
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	// 用另一个插件 ID 的保护器解密应失败
	if _, err := b.Unprotect(ct); err == nil {
		t.Error("不同 pluginID 应派生不同密钥，跨插件解密应失败")
	}
}

// TestDefault_NeverNil 验证 Default 永不返回 nil。
//
// 「密钥库不可用」不应该让插件完全无法保存配置 —— 降级方案在所有平台都可用。
func TestDefault_NeverNil(t *testing.T) {
	ResetDefaultForTest()
	defer ResetDefaultForTest()

	p := Default(testPluginID)
	if p == nil {
		t.Fatal("Default() 返回了 nil，调用方会 panic")
	}
	if p.Scheme() == "" {
		t.Error("Scheme() 不应为空（它会被持久化到 secrets.scheme 列）")
	}
	// 单例语义：多次调用返回同一个
	if Default(testPluginID) != p {
		t.Error("Default() 应返回进程内单例")
	}
}

// TestDefault_RoundTrip 验证平台默认保护器的加解密往返。
// 在 Windows 上这会真实调用 DPAPI。
func TestDefault_RoundTrip(t *testing.T) {
	ResetDefaultForTest()
	defer ResetDefaultForTest()

	p := Default(testPluginID)
	t.Logf("平台 %s 选用方案: %s (secure=%v)", runtime.GOOS, p.Scheme(), p.Secure())

	ct, err := p.Protect([]byte(testSecret))
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	if strings.Contains(ct, testSecret) {
		t.Errorf("密文含明文密钥！")
	}
	got, err := p.Unprotect(ct)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if string(got) != testSecret {
		t.Errorf("往返不一致:\n  原: %q\n  得: %q", testSecret, got)
	}
}

// TestDefault_WindowsUsesDPAPI 验证 Windows 上确实选用了 DPAPI（真加密）。
//
// 这是安全模型的核心承诺：Windows 用户应该拿到与账户绑定的真加密，
// 而不是降级混淆。如果这条测试失败，说明 DPAPI 调用有问题，
// 用户会在不知情的情况下得到弱保护。
func TestDefault_WindowsUsesDPAPI(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅在 Windows 上验证 DPAPI")
	}
	ResetDefaultForTest()
	defer ResetDefaultForTest()

	p := Default(testPluginID)
	if p.Scheme() != SchemeDPAPI {
		t.Errorf("Windows 上应选用 DPAPI，实际为 %q（会降级为弱保护）", p.Scheme())
	}
	if !p.Secure() {
		t.Error("DPAPI 是真加密，Secure() 应返回 true")
	}
}

// TestFingerprint 验证掩码指纹规则。
func TestFingerprint(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"short", "****"},
		{"12345678", "****"},          // 恰好 8 位，全掩码
		{"123456789", "1234****6789"}, // 9 位，保留前4后4
		{"sk-abcdefghijklmnop", "sk-a****mnop"},
		{"中文密钥测试字符串", "中文密钥****试字符串"}, // 按 rune 计数
	}
	for _, c := range cases {
		if got := Fingerprint(c.in); got != c.want {
			t.Errorf("Fingerprint(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// TestFingerprint_NoLeak 验证指纹不泄露密钥内容。
func TestFingerprint_NoLeak(t *testing.T) {
	fp := Fingerprint(testSecret)
	if strings.Contains(fp, testSecret) {
		t.Errorf("指纹泄露完整密钥: %s", fp)
	}
	// 中段绝不能出现
	if strings.Contains(fp, "CDEFGHIJKLMNOP") {
		t.Errorf("指纹泄露密钥中段: %s", fp)
	}
	if !strings.Contains(fp, "****") {
		t.Errorf("指纹缺少掩码标记: %s", fp)
	}
}

// TestMachineID_Stable 验证机器指纹在进程内稳定（缓存生效）。
func TestMachineID_Stable(t *testing.T) {
	a, errA := MachineID()
	b, errB := MachineID()
	if (errA == nil) != (errB == nil) {
		t.Errorf("两次调用错误状态不一致: %v / %v", errA, errB)
	}
	if a != b {
		t.Errorf("机器指纹应稳定: %q vs %q", a, b)
	}
	t.Logf("平台 %s 机器指纹长度=%d err=%v", runtime.GOOS, len(a), errA)
}

// TestNewAccountID_Unique 验证账户标识唯一（碰撞会导致密钥互相覆盖）。
func TestNewAccountID_Unique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := newAccountID()
		if id == "" {
			t.Fatal("账户标识为空")
		}
		if seen[id] {
			t.Fatalf("第 %d 次生成了重复账户标识 %q（会导致密钥互相覆盖）", i, id)
		}
		seen[id] = true
	}
}

// TestIsDecryptError 验证错误判定（含包装情况）。
func TestIsDecryptError(t *testing.T) {
	if !IsDecryptError(ErrDecrypt) {
		t.Error("ErrDecrypt 本身应被识别")
	}
	wrapped := wrapForTest(ErrDecrypt, "上下文")
	if !IsDecryptError(wrapped) {
		t.Error("被包装的 ErrDecrypt 应被识别（errors.Is 语义）")
	}
	if IsDecryptError(nil) {
		t.Error("nil 不应被识别为解密错误")
	}
	if IsDecryptError(bytes.ErrTooLarge) {
		t.Error("无关错误不应被识别为解密错误")
	}
}

func wrapForTest(err error, ctx string) error {
	return &wrappedErr{msg: ctx, inner: err}
}

type wrappedErr struct {
	msg   string
	inner error
}

func (w *wrappedErr) Error() string { return w.msg + ": " + w.inner.Error() }
func (w *wrappedErr) Unwrap() error { return w.inner }
