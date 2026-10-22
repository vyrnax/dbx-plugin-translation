//go:build darwin

package secret

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// readMachineID 通过 ioreg 读取 IOPlatformUUID。
//
// 这是 macOS 的硬件级唯一标识，重装系统也不变，是最稳定的机器指纹。
//
// 命令：ioreg -rd1 -c IOPlatformExpertDevice
// 输出里含一行 "IOPlatformUUID" = "XXXX-XXXX-..."，用字符串解析提取。
//
// 用 exec.Command 参数数组，不拼接 shell。
func readMachineID() (string, error) {
	if _, err := exec.LookPath("ioreg"); err != nil {
		return "", fmt.Errorf("ioreg 命令不可用: %w", err)
	}
	cmd := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("执行 ioreg 失败: %w", err)
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.Contains(line, "IOPlatformUUID") {
			// 形如：    "IOPlatformUUID" = "1234ABCD-..."
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				uuid := strings.Trim(strings.TrimSpace(parts[1]), `"`)
				if uuid != "" {
					return uuid, nil
				}
			}
		}
	}
	return "", fmt.Errorf("ioreg 输出中未找到 IOPlatformUUID")
}
