//go:build linux

package tui

import (
	"os"
	"syscall"
	"unsafe"
)

func ioctl(fd uintptr, request uintptr, value unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(value))
	if errno != 0 {
		return errno
	}
	return nil
}
func IsTerminal(file *os.File) bool {
	if file == nil {
		return false
	}
	var t syscall.Termios
	return ioctl(file.Fd(), syscall.TCGETS, unsafe.Pointer(&t)) == nil
}
func rawTerminal(file *os.File) (func(), error) {
	var before syscall.Termios
	if err := ioctl(file.Fd(), syscall.TCGETS, unsafe.Pointer(&before)); err != nil {
		return nil, err
	}
	raw := before
	raw.Iflag &^= syscall.BRKINT | syscall.ICRNL | syscall.INPCK | syscall.ISTRIP | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Cflag |= syscall.CS8
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.IEXTEN | syscall.ISIG
	raw.Cc[syscall.VMIN] = 0
	raw.Cc[syscall.VTIME] = 1
	if err := ioctl(file.Fd(), syscall.TCSETS, unsafe.Pointer(&raw)); err != nil {
		return nil, err
	}
	return func() { _ = ioctl(file.Fd(), syscall.TCSETS, unsafe.Pointer(&before)) }, nil
}
// lineTerminal reads key by key without echo for the prompt editor. Output
// processing, CR-to-NL input mapping and signal keys keep their usual meaning.
func lineTerminal(file *os.File) (func(), error) {
	var before syscall.Termios
	if err := ioctl(file.Fd(), syscall.TCGETS, unsafe.Pointer(&before)); err != nil {
		return nil, err
	}
	line := before
	line.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.IEXTEN
	line.Cc[syscall.VMIN] = 1
	line.Cc[syscall.VTIME] = 0
	if err := ioctl(file.Fd(), syscall.TCSETS, unsafe.Pointer(&line)); err != nil {
		return nil, err
	}
	return func() { _ = ioctl(file.Fd(), syscall.TCSETS, unsafe.Pointer(&before)) }, nil
}
func terminalSize(file *os.File) (int, int) {
	if file == nil {
		return 80, 24
	}
	var size struct{ Rows, Cols, X, Y uint16 }
	if ioctl(file.Fd(), syscall.TIOCGWINSZ, unsafe.Pointer(&size)) != nil || size.Rows < 5 || size.Cols < 20 {
		return 80, 24
	}
	return int(size.Cols), int(size.Rows)
}
