//go:build windows

package localcli

import (
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// setProcAttr 让子进程成为新进程组的组长。
//
// 这样后续可以用 taskkill /T 一次性终止整棵树。
func setProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

// killTree 终止进程及其全部子孙进程。
//
// 为什么必须杀整棵树：Windows 上执行 .bat / .cmd 会形成
// cmd.exe → 实际命令 的父子结构。只 Kill 直接子进程（cmd.exe）的话，
// 真正的命令（例如 ping、node、python）会变成孤儿进程继续运行，
// 并且【继续持有 stdout 管道的写端】—— 于是读管道的一方永远阻塞，
// 用户点了取消却要等命令自己跑完。
//
// 这是本项目实测踩到的 bug：取消一个 sleep 30 的假 CLI 耗时 29.4 秒。
func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	// /F 强制，/T 含子树。用独立的短超时，避免 taskkill 自身卡住。
	killer := exec.Command("taskkill", "/F", "/T", "/PID", pid)
	_ = killer.Start()
	done := make(chan error, 1)
	go func() { done <- killer.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = killer.Process.Kill()
	}
	// 兜底：taskkill 失败时至少杀掉直接子进程
	_ = cmd.Process.Kill()
}
