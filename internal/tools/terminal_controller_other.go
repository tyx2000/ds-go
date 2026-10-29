//go:build !darwin && !linux

package tools

import (
	"context"
	"errors"
	"time"
)

func startUserTerminal(context.Context, string, string, TerminalShell, int, int, time.Duration) (terminalProcess, error) {
	return nil, errors.New("Host user terminals require a native PTY provider on this platform")
}
