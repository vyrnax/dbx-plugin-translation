//go:build !windows && !darwin && !linux

package secret

import "fmt"

// readMachineID 在未知平台上返回错误，由 deriveKey 用空串继续。
//
// 与 protector_other.go 同理：这个文件保证任何 GOOS 都能编译通过，
// 不会因为缺少 readMachineID 定义而交叉编译失败。
func readMachineID() (string, error) {
	return "", fmt.Errorf("当前平台没有可用的机器指纹来源")
}
