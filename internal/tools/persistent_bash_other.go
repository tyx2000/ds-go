//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package tools

import (
	"errors"
	"os"
	"os/exec"
)

func startBashPTY(cmd *exec.Cmd) (*os.File, error) {
	return nil, errors.New("persistent bash requires a supported Unix PTY")
}
