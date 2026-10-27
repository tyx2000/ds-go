//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd

package state

import (
	"context"
	"errors"
	"os"
)

func lockFile(context.Context, *os.File, bool) error {
	return errors.New("safe session locking is not implemented on this platform")
}
func unlockFile(*os.File) error { return nil }
