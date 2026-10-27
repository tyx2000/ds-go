//go:build !darwin && !linux && !freebsd

package schedule

import "errors"

func daemonLock(string) (func(), error) {
	return nil, errors.New("schedule daemon locking is unsupported on this platform")
}
func lockBusy(error) bool { return false }
