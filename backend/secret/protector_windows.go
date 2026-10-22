//go:build windows

package secret

import (
	"encoding/base64"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// dpapi 用 Windows DPAPI（CryptProtectData / CryptUnprotectData）保护明文。
//
// DPAPI 的安全模型：密钥由【当前 Windows 用户账户】的凭据派生，
// 因此只有同一个用户登录时才能解密。其他用户、其他机器都解不开。
// 这是真加密（Secure() == true），不是混淆。
//
// 为什么用 DPAPI 而不是自己管密钥文件：
//
//	自己管密钥文件等于把密钥和密文放在同一个目录里，
//	攻击者拿到目录就拿到了一切。DPAPI 把密钥托管给操作系统，
//	与用户账户绑定，这是 Windows 上唯一不需要用户交互的真加密方案。
//
// 实现要点：
//   - 纯 syscall，【无 cgo】，不破坏交叉编译
//   - CRYPTPROTECT_UI_FORBIDDEN 禁止弹窗（侧车是无界面后台进程，弹窗会永久阻塞）
//   - 附加 entropy（插件 ID 派生）实现密钥分离：同一台机器上其他用 DPAPI 的程序
//     即使拿到密文也解不开，因为它们不知道我们的 entropy
//   - 返回的内存必须用 LocalFree 释放，否则每次加解密都泄漏一块堆内存
type dpapi struct {
	entropy    *windows.DataBlob
	entropyBuf []byte // 持有 entropy 底层数组，防止 GC 移动
}

// cryptProtectFlags 禁止任何 UI 提示。
//
// 侧车是宿主拉起的无界面子进程，如果 DPAPI 弹窗（例如要求用户确认），
// 没有人能点掉它，请求会永久挂起 —— 表现为「翻译卡死」。
const cryptProtectFlags = windows.CRYPTPROTECT_UI_FORBIDDEN

func newDPAPI(pluginID string) (*dpapi, error) {
	// entropy 用插件 ID 派生，实现密钥分离。
	// 不需要保密（它不是密钥），只需要「其他程序不知道」。
	buf := []byte("dbx-plugin-translation/entropy/v1:" + pluginID)
	blob, err := newDataBlob(buf)
	if err != nil {
		return nil, err
	}
	return &dpapi{entropy: blob, entropyBuf: buf}, nil
}

// newDataBlob 把字节切片包装成 DPAPI 需要的 DataBlob。
//
// 注意：Data 指针直接指向 Go 切片的底层数组。调用方必须保证
// 在 syscall 期间该切片不被 GC 回收或移动 —— 因此 dpapi 结构体
// 用 entropyBuf 字段持有引用。
func newDataBlob(b []byte) (*windows.DataBlob, error) {
	if len(b) == 0 {
		return &windows.DataBlob{}, nil
	}
	return &windows.DataBlob{
		Size: uint32(len(b)),
		Data: &b[0],
	}, nil
}

func (d *dpapi) Scheme() string { return SchemeDPAPI }

// Secure 返回 true —— DPAPI 是与用户账户绑定的真加密。
func (d *dpapi) Secure() bool { return true }

func (d *dpapi) Protect(plaintext []byte) (string, error) {
	if len(plaintext) == 0 {
		return "", fmt.Errorf("不能加密空密钥")
	}
	in, err := newDataBlob(plaintext)
	if err != nil {
		return "", err
	}
	var out windows.DataBlob
	// CryptProtectData(dataIn, name, optionalEntropy, reserved, promptStruct, flags, dataOut)
	// name 传 nil（描述字符串，我们不展示给用户）
	// reserved 与 promptStruct 传 nil（flags 已禁止 UI）
	if err := windows.CryptProtectData(in, nil, d.entropy, 0, nil, cryptProtectFlags, &out); err != nil {
		return "", fmt.Errorf("DPAPI 加密失败: %w", err)
	}
	// 必须释放 DPAPI 分配的内存，否则每次保存引擎都泄漏一块
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))

	result := unsafe.Slice(out.Data, out.Size)
	// 立即拷贝出来：LocalFree 之后原内存就失效了
	copied := make([]byte, len(result))
	copy(copied, result)
	return base64.StdEncoding.EncodeToString(copied), nil
}

func (d *dpapi) Unprotect(ciphertext string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return nil, fmt.Errorf("%w（密文格式非法）", ErrDecrypt)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w（密文为空）", ErrDecrypt)
	}
	in, err := newDataBlob(raw)
	if err != nil {
		return nil, fmt.Errorf("%w（%v）", ErrDecrypt, err)
	}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(in, nil, d.entropy, 0, nil, cryptProtectFlags, &out); err != nil {
		// 常见原因：数据目录被复制到另一台机器，或换了 Windows 用户账户。
		// 归一化为 ErrDecrypt，让上层提示用户重新输入 API Key。
		return nil, fmt.Errorf("%w（DPAPI 解密失败）", ErrDecrypt)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))

	result := unsafe.Slice(out.Data, out.Size)
	copied := make([]byte, len(result))
	copy(copied, result)
	return copied, nil
}

// platformProtector 是 choose() 调用的平台入口。
// 返回 error 时 choose() 会降级到机器绑定 AES-GCM。
func platformProtector(pluginID string) (Protector, error) {
	return newDPAPI(pluginID)
}
