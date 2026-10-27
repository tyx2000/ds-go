//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd

package mcp

import (
	"errors"
	"os/exec"
)

func setupProcess(*exec.Cmd) error {
	return errors.New("safe MCP process-tree cleanup is not implemented on this platform")
}
func signalProcess(cmd *exec.Cmd, kill bool) error { return cmd.Process.Kill() }
