//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package context

import (
	"errors"
	"os"
	"syscall"
)

func openInstructionFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
func isInstructionNotDirectory(err error) bool { return errors.Is(err, syscall.ENOTDIR) }
