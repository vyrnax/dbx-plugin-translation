//go:build darwin

package secret

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os/exec"
	"strings"
)

// keychain 用 macOS Keychain 保护明文。
//
// 通过 `security` 命令访问（macOS 自带，无需安装）。
// 这是真加密：密钥托管在 Keychain 里，受用户登录密码与 ACL 保护。
//
// 实现要点：
//   - 一律用 exec.Command 的【参数数组】，绝不拼接 shell 字符串。
//     API Key 可能含 $ ` " \ 等字符，拼接会导致命令注入或语法错误。
//   - 密文用 base64 存入 Keychain 的 password 字段，避免二进制数据
//     在命令行参数里出问题。
//   - 每个引擎一条 Keychain 记录，service 固定、account 用引擎 ID，
//     这样多个引擎互不覆盖。
//
// 局限：Keychain 访问可能触发系统授权弹窗（首次访问时）。
// 侧车是无界面进程，弹窗会阻塞。因此 add/find 都带 -T "" 限制 ACL，
// 并在失败时降级到机器绑定方案（由 choose() 处理）。
type keychain struct {
	service string
}

const keychainService = "dbx-plugin-translation"

func newKeychain(pluginID string) (*keychain, error) {
	// 探测 security 命令是否可用（理论上 macOS 一定有，但容器/精简系统可能没有）
	if _, err := exec.LookPath("security"); err != nil {
		return nil, fmt.Errorf("security 命令不可用: %w", err)
	}
	return &keychain{service: keychainService + ":" + pluginID}, nil
}

func (k *keychain) Scheme() string { return SchemeKeychain }

// Secure 返回 true —— Keychain 是系统级真加密。
func (k *keychain) Secure() bool { return true }

// Protect 把明文写入 Keychain，返回一个【引用令牌】而非密文本身。
//
// 这是与 DPAPI 的关键差异：DPAPI 返回自包含的密文，
// 而 Keychain 把数据存在系统里，我们只保存「怎么找到它」。
// 引用令牌格式：keychain:<account>
//
// 好处是密文根本不落我们的磁盘；代价是数据目录单独拷走时密钥丢失
// （这其实是正确的安全行为）。
func (k *keychain) Protect(plaintext []byte) (string, error) {
	if len(plaintext) == 0 {
		return "", fmt.Errorf("不能加密空密钥")
	}
	account := newAccountID()
	encoded := base64.StdEncoding.EncodeToString(plaintext)

	// -U 表示已存在则更新（避免重复保存同一引擎时报错）
	// -T "" 限制 ACL，减少授权弹窗
	cmd := exec.Command("security", "add-generic-password",
		"-U",
		"-s", k.service,
		"-a", account,
		"-w", encoded,
		"-T", "",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// 不把 stderr 原样返回：它可能含密钥片段
		return "", fmt.Errorf("写入 Keychain 失败: %w", redactExecErr(err, stderr.String()))
	}
	return "keychain:" + account, nil
}

func (k *keychain) Unprotect(reference string) ([]byte, error) {
	account := strings.TrimPrefix(strings.TrimSpace(reference), "keychain:")
	if account == "" || account == reference {
		return nil, fmt.Errorf("%w（引用格式非法）", ErrDecrypt)
	}
	cmd := exec.Command("security", "find-generic-password",
		"-s", k.service,
		"-a", account,
		"-w",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// errSecItemNotFound（-25300）是最常见原因：Keychain 里没有这条记录
		return nil, fmt.Errorf("%w（Keychain 读取失败）", ErrDecrypt)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(stdout.String()))
	if err != nil {
		return nil, fmt.Errorf("%w（Keychain 内容格式非法）", ErrDecrypt)
	}
	return raw, nil
}

// redactExecErr 清洗子进程错误信息，避免把密钥片段带进错误消息。
//
// security 命令失败时可能在 stderr 里回显 -w 参数的值（即 base64 后的密钥），
// 因此这里只保留错误类别，不保留原文。
func redactExecErr(err error, stderr string) error {
	msg := strings.TrimSpace(stderr)
	switch {
	case strings.Contains(msg, "errSecDuplicateItem"), strings.Contains(msg, "-25299"):
		return fmt.Errorf("记录已存在")
	case strings.Contains(msg, "errSecItemNotFound"), strings.Contains(msg, "-25300"):
		return fmt.Errorf("记录不存在")
	case strings.Contains(msg, "errSecAuthFailed"), strings.Contains(msg, "-25293"):
		return fmt.Errorf("授权失败")
	case strings.Contains(msg, "interaction is not allowed"), strings.Contains(msg, "-25308"):
		return fmt.Errorf("Keychain 被锁定或不允许交互")
	default:
		// 未知错误：只返回 err 本身，绝不返回 stderr 原文
		return err
	}
}

func platformProtector(pluginID string) (Protector, error) {
	return newKeychain(pluginID)
}
