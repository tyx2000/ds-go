//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package tools

import "os/exec"

func configureProcess(cmd *exec.Cmd) {}
func terminateProcess(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
