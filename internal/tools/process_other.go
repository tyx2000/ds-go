//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package tools

import "os/exec"

func configureProcess(cmd *exec.Cmd) {}
