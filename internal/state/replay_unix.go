//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package state

import (
	"os"
	"syscall"
)

func openReplayKey(root *os.Root) (*os.File, error) {
	return root.OpenFile(replayKeyName, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

func syncReplayDirectory(f *os.File) error { return f.Sync() }
