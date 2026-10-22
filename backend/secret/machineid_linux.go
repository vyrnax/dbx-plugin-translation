//go:build linux

package secret

import (
	"fmt"
	"os"
	"strings"
)

// readMachineID 读取 systemd 的 machine-id。
//
// 优先 /etc/machine-id（systemd 标准位置），
// 回退 /var/lib/dbus/machine-id（老系统或只有 D-Bus 的环境）。
//
// 容器里这两个文件可能都不存在或为空 —— 此时返回错误，
// 由 deriveKey 用空串继续（Secure() 仍是 false，警示条会显示）。
func readMachineID() (string, error) {
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		id := strings.TrimSpace(string(b))
		if id != "" {
			return id, nil
		}
	}
	return "", fmt.Errorf("未找到 machine-id（/etc/machine-id 与 /var/lib/dbus/machine-id 均不可用）")
}
