//go:build darwin

package preview

import (
	"os"
	"syscall"
	"unsafe"
)

func isTTY(f *os.File) bool {
	var state syscall.Termios
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGETA, uintptr(unsafe.Pointer(&state)))
	return err == 0
}
