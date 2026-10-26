//go:build !windows

package localcli

import (
	"os/exec"
	"syscall"
)

// setProcAttr 让子进程成为新进程组的组长（Setpgid）。
//
// 这样 killTree 可以用负 PID 一次性终止整个进程组。
func setProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killTree 终止进程组（含全部子孙进程）。
//
// 与 Windows 同理：shell 脚本会形成 sh → 实际命令 的父子结构，
// 只杀直接子进程会留下孤儿进程继续持有 stdout 管道，
// 导致读管道的一方永远阻塞、取消无法及时生效。
//
// 用负 PID 表示「整个进程组」。Setpgid 保证了子进程自成一个组，
// 因此不会误杀侧车自己。
func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// 先尝试杀进程组；失败（例如 Setpgid 未生效）则退回杀单个进程
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
