//go:build linux

package preview

import (
	"os"
	"syscall"
	"unsafe"
)

func isTTY(f *os.File) bool {
	var state syscall.Termios
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&state)))
	return err == 0
}
