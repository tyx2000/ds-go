//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"dsgo/internal/core"
)

func TestPersistentBashStateIsolationAndCancellation(t *testing.T) {
	r := New()
	defer r.Close()
	tc := core.ToolContext{SessionID: "one", CWD: t.TempDir(), Permission: "full-access"}
	run := func(command string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result, err := r.Execute(ctx, tc, "bash", core.JSON(map[string]any{"command": command}))
		if err != nil {
			t.Fatal(err)
		}
		return result.Content
	}
	if got := run("export DSGO_BASH_TEST_VALUE=kept; mkdir sub; cd sub; printf hello"); !strings.Contains(got, "hello\n[Command finished with exit code 0]") {
		t.Fatalf("initial: %q", got)
	}
	if got := run("printf '%s:%s' \"$DSGO_BASH_TEST_VALUE\" \"${PWD##*/}\""); !strings.Contains(got, "kept:sub") {
		t.Fatalf("state: %q", got)
	}
	tc.SessionID = "two"
	if got := run("printf 'value=%s' \"${DSGO_BASH_TEST_VALUE-unset}\""); !strings.Contains(got, "value=unset") {
		t.Fatalf("cross-session environment: %q", got)
	}
	tc.SessionID = "one"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	_, err := r.Execute(ctx, tc, "bash", core.JSON(map[string]any{"command": "sleep 10"}))
	cancel()
	if err == nil {
		t.Fatal("cancelled command succeeded")
	}
	if got := run("printf 'value=%s' \"${DSGO_BASH_TEST_VALUE-unset}\""); !strings.Contains(got, "value=unset") {
		t.Fatalf("cancel did not reset shell: %q", got)
	}
}

func TestPersistentBashLargeOutputSettles(t *testing.T) {
	r := New()
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tc := core.ToolContext{SessionID: "large", CWD: t.TempDir(), Permission: "full-access"}
	result, err := r.Execute(ctx, tc, "bash", core.JSON(map[string]any{"command": "printf '%100000s' x"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "<response clipped>") || !strings.HasSuffix(result.Content, "[Command finished with exit code 0]") {
		t.Fatalf("large output did not settle (%d bytes)", len(result.Content))
	}
}

func TestWorkingDirectoryCannotExpandFileWriteBoundary(t *testing.T) {
	r := New()
	defer r.Close()
	root, outside := t.TempDir(), t.TempDir()
	tc := core.ToolContext{SessionID: "cd", CWD: outside, WorkspaceRoot: root, Permission: "workspace-write", ApprovalPolicy: "never"}
	if _, err := r.Execute(context.Background(), tc, "write_file", core.JSON(map[string]any{"path": "escape.txt", "content": "no"})); err == nil {
		t.Fatal("changing cwd expanded writable roots")
	}
}
