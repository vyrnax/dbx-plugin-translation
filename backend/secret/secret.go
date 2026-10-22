// Package secret 负责 API Key 的加密保管。
//
// # 为什么需要这一层
//
// 本插件【不使用】DBX 的 connection-provider，因此拿不到宿主的 Secret Store。
// 引擎配置由插件自己管理，API Key 必须自己加密 —— 明文落盘是不可接受的：
// 插件数据目录是普通用户可读的目录，任何本地进程（包括恶意软件）都能读到。
//
// # 分层策略
//
//	优先级 1：系统密钥库（真加密）
//	  Windows  → DPAPI（CryptProtectData，绑定当前用户账户）
//	  macOS    → Keychain（security 命令）
//	  Linux    → libsecret（secret-tool 命令）
//
//	优先级 2：机器绑定 AES-256-GCM（仅混淆）
//	  密钥 = HKDF-SHA256(machineID + pluginID + salt)
//
// 降级方案【不是真加密】：能读磁盘的本地进程同样能读到 machineID，
// 因此也能派生出密钥并解密。它的价值只在于「防止密钥以明文形式直接出现在
// 文件里」—— 避免用户误把数据目录截图/分享时泄露，也避免被简单的
// 字符串扫描工具直接抓到。
//
// 因此降级时必须【明确告知用户】：Protector.Secure() 返回 false，
// 侧车通过 translate/ping 的 secretSecure 字段传给前端，
// 前端在引擎管理页顶部常驻黄色警示条。绝不静默降级。
//
// # 密钥流转铁律
//
//  1. 明文只在「加密前」与「构造 HTTP 请求头时」两个瞬间存在
//  2. 绝不写入日志、事件、错误消息或任何 RPC 返回值
//  3. RPC 只返回 Fingerprint（掩码指纹），永不回传明文
//  4. 解密失败返回 ErrDecrypt，提示用户重新输入，【不静默清空】配置
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// ErrDecrypt 表示密文无法解密。
//
// 常见原因：
//   - 系统密钥库不可用（例如 Linux 未装 libsecret，或 macOS Keychain 被锁）
//   - 数据目录被复制到另一台机器（machineID 变了，降级方案的密钥派生不出来）
//   - 密文被手工篡改或损坏
//
// 调用方应把它映射为错误码 -32035，并提示「密钥库不可用或机器已变更，
// 请重新输入 API Key」。【绝不】静默清空配置 —— 那会让用户以为配置还在。
var ErrDecrypt = errors.New("密钥解密失败：系统密钥库不可用，或数据已迁移到其它机器")

// Protector 是密钥保护器的统一接口。
type Protector interface {
	// Scheme 返回方案标识，用于持久化到 secrets.scheme 列，
	// 以便解密时知道该用哪个实现。
	// 取值：dpapi | keychain | secret-tool | aesgcm-machine
	Scheme() string

	// Secure 返回是否达到「真加密」级别。
	// false 表示只是机器绑定混淆，前端必须据此显示安全警示。
	Secure() bool

	// Protect 加密明文，返回可持久化的字符串（通常是 base64）。
	Protect(plaintext []byte) (string, error)

	// Unprotect 解密 Protect 产出的字符串。
	// 失败时返回 ErrDecrypt（或包装了它的错误）。
	Unprotect(ciphertext string) ([]byte, error)
}

// 已知的 scheme 标识。持久化后不可更改，否则旧数据无法解密。
const (
	SchemeDPAPI         = "dpapi"
	SchemeKeychain      = "keychain"
	SchemeSecretTool    = "secret-tool"
	SchemeAESGCMMachine = "aesgcm-machine"
)

var (
	defaultOnce sync.Once
	defaultProt Protector
)

// Default 返回当前平台的密钥保护器（进程内单例）。
//
// 选择逻辑：先试平台密钥库，不可用则降级到机器绑定 AES-GCM。
// 「不可用」的判定由各平台实现自己负责（例如检测 secret-tool 是否在 PATH 里）。
//
// 本函数【永不返回 nil】—— 降级方案在所有平台都可用，
// 因此调用方不需要处理 nil，也不会因为密钥库缺失而无法启动。
func Default(pluginID string) Protector {
	defaultOnce.Do(func() {
		defaultProt = choose(pluginID)
	})
	return defaultProt
}

// choose 按平台选择保护器，失败则降级。
//
// platformProtector 由各平台的构建标签文件提供（protector_windows.go 等）。
func choose(pluginID string) Protector {
	if p, err := platformProtector(pluginID); err == nil && p != nil {
		return p
	}
	// 降级：机器绑定 AES-GCM。
	// 即使 machineID 取不到也要返回一个可用的保护器（用空 machineID），
	// 因为「密钥库完全不可用」不应该让插件彻底无法保存配置 ——
	// 但此时 Secure() 仍是 false，警示条会照常显示。
	return newMachineBound(pluginID)
}

// ResetDefaultForTest 重置单例，仅供测试使用。
//
// 生产代码不应调用 —— Default 的单例语义保证了整个进程用同一套加密方案，
// 中途切换会导致已保存的密文无法解密。
func ResetDefaultForTest() {
	defaultOnce = sync.Once{}
	defaultProt = nil
}

// ---------------------------------------------------------------------------
// 机器绑定 AES-256-GCM（降级方案）
// ---------------------------------------------------------------------------

// machineBound 用「机器指纹派生密钥 + AES-256-GCM」保护明文。
//
// 安全边界（必须诚实告知用户）：
//
//	这不是真加密。machineID 对本地任何进程都可读，
//	因此能读磁盘的攻击者同样能派生密钥并解密。
//	它只防止「明文直接出现在文件里」这一种最粗浅的泄露。
type machineBound struct {
	pluginID string
	key      []byte // 32 字节 AES-256 密钥，进程内缓存
	keyErr   error
}

// hkdfSalt 是 HKDF 的固定盐值。
//
// 用固定值而非随机值，是因为密钥必须可重现 —— 每次启动都要能派生出
// 同一把密钥来解密已保存的数据。随机盐需要额外持久化，
// 而持久化的盐同样对攻击者可见，并不能提升安全性。
var hkdfSalt = []byte("dbx-plugin-translation/secret/v1")

func newMachineBound(pluginID string) *machineBound {
	m := &machineBound{pluginID: pluginID}
	m.key, m.keyErr = deriveKey(pluginID)
	return m
}

// deriveKey 用 HKDF-SHA256 从 machineID + pluginID 派生 32 字节 AES 密钥。
//
// 为什么用 HKDF 而不是直接 SHA256(machineID)：
//
//	machineID 的熵可能很低（例如某些虚拟机的 machine-id 是可预测的），
//	HKDF 的 extract-then-expand 结构能更好地把有限熵扩散成均匀密钥，
//	并且通过 info 参数（pluginID）实现密钥分离 ——
//	即使同一台机器上有多个插件用同样方式派生，密钥也互不相同。
func deriveKey(pluginID string) ([]byte, error) {
	mid, err := MachineID()
	if err != nil {
		// machineID 取不到时用空串继续 —— 此时密钥只由 pluginID + salt 决定，
		// 安全性进一步下降，但 Secure() 本来就是 false，警示条会显示。
		// 不返回错误是为了保证「密钥库不可用」不会让插件完全无法工作。
		mid = ""
	}
	ikm := []byte("machine:" + mid + "|plugin:" + pluginID)
	// 用本包 hkdf.go 里的标准库实现，不引入 golang.org/x/crypto
	// （x/crypto 最新版要求 go 1.26.0，会顶高 go.mod 的工具链下限）。
	key, err := hkdfSHA256(ikm, hkdfSalt, []byte("aes-256-gcm"), 32)
	if err != nil {
		return nil, fmt.Errorf("派生密钥失败: %w", err)
	}
	return key, nil
}

func (m *machineBound) Scheme() string { return SchemeAESGCMMachine }

// Secure 恒为 false —— 机器绑定方案只是混淆，不是真加密。
// 前端必须据此显示安全警示条。
func (m *machineBound) Secure() bool { return false }

// Protect 用 AES-256-GCM 加密，输出格式：base64(nonce || ciphertext || tag)。
//
// nonce 随机生成并前置存储。GCM 的 nonce 绝不能重复使用（重复会彻底破坏
// 机密性与完整性），因此每次加密都用 crypto/rand 生成新的 12 字节 nonce。
func (m *machineBound) Protect(plaintext []byte) (string, error) {
	if m.keyErr != nil {
		return "", m.keyErr
	}
	// 拒绝空明文：加密一个空密钥没有意义，而且会掩盖上层的配置错误
	// （例如表单没填 API Key 却走到了保存流程）。
	// 所有 Protector 实现都必须有这个检查，否则行为不一致会让
	// 「换台机器后突然能保存空密钥」这类问题极难排查。
	if len(plaintext) == 0 {
		return "", fmt.Errorf("不能加密空密钥")
	}
	block, err := aes.NewCipher(m.key)
	if err != nil {
		return "", fmt.Errorf("初始化加密器失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("初始化 GCM 失败: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("生成随机 nonce 失败: %w", err)
	}
	// Seal 把 tag 追加到密文后面；再把 nonce 前置，得到自描述的密文块
	sealed := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Unprotect 解密 Protect 的输出。
//
// 任何失败都归一化为 ErrDecrypt（包装原始错误），因为对调用方来说
// 「密钥库不可用」「机器变了」「密文损坏」的处理方式完全一样：
// 提示用户重新输入 API Key。区分具体原因只会泄露实现细节。
func (m *machineBound) Unprotect(ciphertext string) ([]byte, error) {
	if m.keyErr != nil {
		return nil, fmt.Errorf("%w（密钥派生失败）", ErrDecrypt)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(ciphertext))
	if err != nil {
		return nil, fmt.Errorf("%w（密文格式非法）", ErrDecrypt)
	}
	block, err := aes.NewCipher(m.key)
	if err != nil {
		return nil, fmt.Errorf("%w（初始化加密器失败）", ErrDecrypt)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%w（初始化 GCM 失败）", ErrDecrypt)
	}
	ns := gcm.NonceSize()
	if len(raw) < ns+gcm.Overhead() {
		return nil, fmt.Errorf("%w（密文长度不足）", ErrDecrypt)
	}
	nonce, body := raw[:ns], raw[ns:]
	plain, err := gcm.Open(nil, nonce, body, nil)
	if err != nil {
		// GCM 的 Open 失败意味着 tag 校验不通过 —— 要么密钥不对（机器变了），
		// 要么密文被篡改。两种情况都必须拒绝，绝不能返回部分数据。
		return nil, fmt.Errorf("%w（校验失败）", ErrDecrypt)
	}
	return plain, nil
}

// ---------------------------------------------------------------------------
// 指纹
// ---------------------------------------------------------------------------

// Fingerprint 生成密钥的掩码指纹，用于 UI 展示「已配置」而不泄露内容。
//
// 规则：
//   - 空串返回空串（前端据此判断 hasSecret）
//   - 长度 <= 8：全部掩码为 ****（太短时保留任何片段都等于泄露）
//   - 否则保留前 4 后 4，中间用 **** 代替
//
// 前 4 后 4 对 sk- 前缀的 OpenAI 风格 Key 是安全的（前缀是公开信息），
// 但对短 Key 不安全，所以有 <=8 全掩码的分支。
func Fingerprint(s string) string {
	v := strings.TrimSpace(s)
	if v == "" {
		return ""
	}
	r := []rune(v)
	if len(r) <= 8 {
		return "****"
	}
	return string(r[:4]) + "****" + string(r[len(r)-4:])
}

// IsDecryptError 判断错误是否为解密失败（含被包装的情况）。
// 调用方据此映射为错误码 -32035。
func IsDecryptError(err error) bool {
	return errors.Is(err, ErrDecrypt)
}
