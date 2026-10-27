//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package tools

import (
	"context"
	"dsgo/internal/core"
	"encoding/json"
	"errors"
)

func (r *Registry) terminalTool(context.Context, core.ToolContext, json.RawMessage) (core.ToolResult, error) {
	return core.ToolResult{}, errors.New("PTY terminal backend unavailable on this platform")
}
