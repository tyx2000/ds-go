//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package tools

import (
	"os"
	"os/exec"
	"syscall"
)

func configureProcess(cmd *exec.Cmd) {
	// A new session has no controlling terminal: even a read-only command
	// cannot open /dev/tty to inject terminal output or race the harness for
	// keyboard input. pgid still equals pid, so kill(-pid) cleans the group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
}
