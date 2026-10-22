//go:build !windows && !darwin && !linux

package secret

import "fmt"

// platformProtector 在未知平台上直接返回错误，由 choose() 降级到机器绑定方案。
//
// 这个文件的存在是【交叉编译的硬性要求】：
// 如果只有 windows/darwin/linux 三个构建标签文件，那么为其他平台
// （例如 freebsd、js/wasm）编译时会因为「platformProtector 未定义」而失败。
// 提供一个兜底实现保证任何 GOOS 都能编译通过。
func platformProtector(pluginID string) (Protector, error) {
	return nil, fmt.Errorf("当前平台没有可用的系统密钥库")
}
