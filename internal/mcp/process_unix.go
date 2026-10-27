//go:build darwin || linux || freebsd || openbsd || netbsd

package mcp

import (
	"os/exec"
	"syscall"
)

func setupProcess(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}
func signalProcess(cmd *exec.Cmd, kill bool) error {
	signal := syscall.SIGTERM
	if kill {
		signal = syscall.SIGKILL
	}
	return syscall.Kill(-cmd.Process.Pid, signal)
}
