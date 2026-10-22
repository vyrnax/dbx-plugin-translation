package secret

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

// machineIDCache 缓存机器指纹，避免每次派生密钥都去读注册表/文件。
//
// 机器指纹在进程生命周期内不会变（变了也意味着数据本来就该解不开），
// 因此缓存是安全的，且能省掉重复的系统调用。
var (
	machineOnce sync.Once
	machineVal  string
	machineErr  error
)

// MachineID 返回当前机器的稳定指纹。
//
// 用途：机器绑定 AES-GCM 降级方案的密钥派生输入。
//
// 各平台来源（由 machineid_<os>.go 提供）：
//
//	Windows → 注册表 HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid
//	Linux   → /etc/machine-id（或 /var/lib/dbus/machine-id）
//	macOS   → IOPlatformUUID（ioreg 命令）
//
// 失败时返回错误，调用方（deriveKey）会用空串继续 ——
// 此时安全性进一步下降，但 Secure() 本来就是 false，警示条会显示。
// 「机器指纹取不到」不应该让插件完全无法保存配置。
func MachineID() (string, error) {
	machineOnce.Do(func() {
		machineVal, machineErr = readMachineID()
	})
	return machineVal, machineErr
}

// ResetMachineIDForTest 重置缓存，仅供测试使用。
func ResetMachineIDForTest() {
	machineOnce = sync.Once{}
	machineVal = ""
	machineErr = nil
}

// newAccountID 生成一个随机账户标识，用于系统密钥库（Keychain / libsecret）
// 里区分不同引擎的记录。
//
// 为什么用随机 ID 而不是引擎 ID：
//
//	引擎 ID 是用户可见的、可能含语义信息的字符串，直接拿去做 Keychain 的
//	account 会把「用户有哪些引擎」这类元数据暴露给系统密钥库。
//	用随机 ID 则密钥库里只有一串无意义的标识，元数据仍留在我们自己的
//	SQLite 里（secrets 表的 engine_id → cipher 引用令牌映射）。
//
// 16 字节随机数（128 位熵）足以避免碰撞。
func newAccountID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败是极严重的系统问题，此时退化为时间戳+计数器。
		// 不用 panic —— 侧车 panic 会让宿主直接丢弃它，表现为「插件坏了」。
		return fallbackAccountID()
	}
	return hex.EncodeToString(b)
}

// fallbackCounter 是 crypto/rand 不可用时的退化计数器。
var fallbackCounter uint64
var fallbackMu sync.Mutex

func fallbackAccountID() string {
	fallbackMu.Lock()
	defer fallbackMu.Unlock()
	fallbackCounter++
	// 退化方案：纳秒时间戳 + 单调计数器，仍能保证进程内唯一
	return "fb-" + hex.EncodeToString([]byte{
		byte(fallbackCounter >> 56), byte(fallbackCounter >> 48),
		byte(fallbackCounter >> 40), byte(fallbackCounter >> 32),
		byte(fallbackCounter >> 24), byte(fallbackCounter >> 16),
		byte(fallbackCounter >> 8), byte(fallbackCounter),
	})
}
