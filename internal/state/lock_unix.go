//go:build darwin || linux || freebsd || openbsd || netbsd

package state

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

func lockFile(ctx context.Context, f *os.File, wait bool) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		if !wait {
			return ErrBusy
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func unlockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
