//go:build darwin || linux

package reference

import (
	"os"
	"syscall"
)

func openFileDirectory(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_DIRECTORY, 0)
}
