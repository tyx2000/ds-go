//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"dsgo/internal/core"
)

func TestPTYInteractiveEchoResizeAndCancellation(t *testing.T) {
	r := New()
	defer r.Close()
	tc := toolContext(t)
	tc.Permission = "full-access"
	res, err := r.Execute(context.Background(), tc, "terminal", core.JSON(map[string]any{"action": "start", "rows": 31, "cols": 101, "command": "test -t 0 && printf 'tty-ready\\n'; stty size; IFS= read -r line; stty size; printf 'received:%s\\n' \"$line\"; IFS= read -r next"}))
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skip("outer sandbox prohibits PTY creation")
		}
		t.Fatal(err)
	}
	var value struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(res.Content), &value); err != nil {
		t.Fatal(err)
	}
	readUntil := func(text string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			res := call(t, r, tc, "terminal", map[string]any{"action": "read", "id": value.ID, "wait_ms": 100})
			if strings.Contains(res.Content, text) {
				return
			}
		}
		t.Fatalf("terminal did not emit %q", text)
	}
	readUntil("tty-ready")
	readUntil("31 101")
	call(t, r, tc, "terminal", map[string]any{"action": "resize", "id": value.ID, "rows": 42, "cols": 120})
	call(t, r, tc, "terminal", map[string]any{"action": "write", "id": value.ID, "input": "echoed\n"})
	readUntil("42 120")
	readUntil("received:echoed")
	intruder := tc
	intruder.SessionID = "other"
	if _, err := r.Execute(context.Background(), intruder, "terminal", core.JSON(map[string]any{"action": "write", "id": value.ID, "input": "bad\n"})); err == nil {
		t.Fatal("cross-session PTY write succeeded")
	}
	started := time.Now()
	res = call(t, r, tc, "terminal", map[string]any{"action": "kill", "id": value.ID})
	if !strings.Contains(res.Content, `"status":"finished"`) || time.Since(started) > 2*time.Second {
		t.Fatalf("PTY cancellation failed: %s", res.Content)
	}
}
