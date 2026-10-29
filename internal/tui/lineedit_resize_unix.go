//go:build darwin || linux

package tui

import (
	"os"
	"os/signal"
	"syscall"
)

func watchLineResize(resize func()) func() {
	changes := make(chan os.Signal, 1)
	done, stopped := make(chan struct{}), make(chan struct{})
	signal.Notify(changes, syscall.SIGWINCH)
	go func() {
		defer close(stopped)
		for {
			select {
			case <-done:
				return
			case <-changes:
				resize()
			}
		}
	}()
	return func() { signal.Stop(changes); close(done); <-stopped }
}
