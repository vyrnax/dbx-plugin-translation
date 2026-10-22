//go:build windows

package secret

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// readMachineID 从注册表读取 MachineGuid。
//
// 这是 Windows 安装时生成的全局唯一标识，重装系统才会变，
// 是 Windows 上最稳定的机器指纹来源。
//
// 路径：HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid
// 该键对普通用户可读（不需要管理员权限），因此侧车能直接访问。
func readMachineID() (string, error) {
	const path = `SOFTWARE\Microsoft\Cryptography`
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.READ|registry.WOW64_64KEY)
	if err != nil {
		return "", fmt.Errorf("打开注册表失败: %w", err)
	}
	defer func() { _ = k.Close() }()

	guid, _, err := k.GetStringValue("MachineGuid")
	if err != nil {
		return "", fmt.Errorf("读取 MachineGuid 失败: %w", err)
	}
	guid = strings.TrimSpace(guid)
	if guid == "" {
		return "", fmt.Errorf("MachineGuid 为空")
	}
	return guid, nil
}
