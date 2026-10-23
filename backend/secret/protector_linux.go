//go:build linux

package secret

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os/exec"
	"strings"
)

// secretTool 用 libsecret（secret-tool 命令）保护明文。
//
// 这是 Linux 桌面环境的标准密钥库（GNOME Keyring / KDE Wallet 都实现了
// org.freedesktop.secrets 接口）。属于真加密。
//
// 局限（必须诚实处理）：
//   - secret-tool 需要【单独安装】（libsecret-tools 包），很多系统默认没有
//   - 需要一个正在运行的 D-Bus 会话与密钥环守护进程
//   - 无头服务器 / 容器里通常不可用
//
// 因此 newSecretTool 会主动探测可用性，不可用时返回 error，
// 由 choose() 降级到机器绑定 AES-GCM，并通过 Secure()==false 告知前端显示警示。
type secretTool struct {
	attribute string
}

func newSecretTool(pluginID string) (*secretTool, error) {
	if _, err := exec.LookPath("secret-tool"); err != nil {
		return nil, fmt.Errorf("secret-tool 未安装（需要 libsecret-tools）: %w", err)
	}
	return &secretTool{attribute: "dbx-plugin-translation:" + pluginID}, nil
}

func (s *secretTool) Scheme() string { return SchemeSecretTool }

// Secure 返回 true —— libsecret 是系统级真加密。
func (s *secretTool) Secure() bool { return true }

// Protect 把明文存入密钥环，返回引用令牌 secrettool:<account>。
//
// 与 macOS Keychain 同理：密文不落我们的磁盘，只保存查找方式。
func (s *secretTool) Protect(plaintext []byte) (string, error) {
	if len(plaintext) == 0 {
		return "", fmt.Errorf("不能加密空密钥")
	}
	account := newAccountID()
	encoded := base64.StdEncoding.EncodeToString(plaintext)

	// secret-tool store --label=<label> <attr> <value> ...
	// 密钥内容从 stdin 读入（而不是命令行参数），避免出现在 ps 输出里 ——
	// 命令行参数对同一台机器上的所有用户都可见，这是真实的泄露渠道。
	cmd := exec.Command("secret-tool", "store",
		"--label=DBX Translation 即时翻译 API Key",
		"service", s.attribute,
		"account", account,
	)
	cmd.Stdin = strings.NewReader(encoded)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("写入密钥环失败: %w", redactSecretToolErr(err, stderr.String()))
	}
	return "secrettool:" + account, nil
}

func (s *secretTool) Unprotect(reference string) ([]byte, error) {
	account := strings.TrimPrefix(strings.TrimSpace(reference), "secrettool:")
	if account == "" || account == reference {
		return nil, fmt.Errorf("%w（引用格式非法）", ErrDecrypt)
	}
	cmd := exec.Command("secret-tool", "lookup",
		"service", s.attribute,
		"account", account,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w（密钥环读取失败）", ErrDecrypt)
	}
	// lookup 找不到时返回空输出且退出码为 0，必须单独判断
	out := strings.TrimSpace(stdout.String())
	if out == "" {
		return nil, fmt.Errorf("%w（密钥环中无此记录）", ErrDecrypt)
	}
	raw, err := base64.StdEncoding.DecodeString(out)
	if err != nil {
		return nil, fmt.Errorf("%w（密钥环内容格式非法）", ErrDecrypt)
	}
	return raw, nil
}

// redactSecretToolErr 清洗错误信息，绝不返回 stderr 原文（可能含密钥）。
func redactSecretToolErr(err error, stderr string) error {
	msg := strings.TrimSpace(stderr)
	switch {
	case strings.Contains(msg, "Cannot autolaunch D-Bus"), strings.Contains(msg, "dbus"):
		return fmt.Errorf("没有可用的 D-Bus 会话（无头环境？）")
	case strings.Contains(msg, "prompt"), strings.Contains(msg, "locked"):
		return fmt.Errorf("密钥环被锁定或需要交互授权")
	default:
		return err
	}
}

func platformProtector(pluginID string) (Protector, error) {
	return newSecretTool(pluginID)
}
