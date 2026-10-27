//go:build !darwin && !linux

package tui

import (
	"errors"
	"os"
)

func IsTerminal(*os.File) bool { return false }
func rawTerminal(*os.File) (func(), error) {
	return nil, errors.New("interactive terminal pager unavailable on this platform")
}
func lineTerminal(*os.File) (func(), error) {
	return nil, errors.New("prompt line editing unavailable on this platform")
}
func terminalSize(*os.File) (int, int) { return 80, 24 }
