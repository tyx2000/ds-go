//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package tools

import (
	"github.com/creack/pty"
	"os"
	"os/exec"
)

func startBashPTY(cmd *exec.Cmd) (*os.File, error) {
	cmd.SysProcAttr.Setpgid = false
	return pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 120})
}
