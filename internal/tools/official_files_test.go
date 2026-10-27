package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dsgo/internal/core"
)

func TestOfficialFilesObservationAndFreshness(t *testing.T) {
	r := New()
	defer r.Close()
	tc := core.ToolContext{SessionID: "one", CWD: t.TempDir(), Permission: "workspace-write", ApprovalPolicy: "never"}
	call := func(name string, args any) (core.ToolResult, error) {
		return r.Execute(context.Background(), tc, name, core.JSON(args))
	}
	path := filepath.Join(tc.CWD, "existing")
	if err := os.WriteFile(path, []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := call("write", map[string]any{"file_path": path, "content": "overwrite"}); err == nil || !strings.Contains(err.Error(), "FS_NOT_OBSERVED") {
		t.Fatalf("unobserved overwrite: %v", err)
	}
	if _, err := call("edit", map[string]any{"file_path": path, "old_string": "before", "new_string": "after"}); err == nil || !strings.Contains(err.Error(), "FS_NOT_OBSERVED") {
		t.Fatalf("unobserved edit: %v", err)
	}
	if _, err := call("read", map[string]any{"file_path": path}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("externally changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := call("write", map[string]any{"file_path": path, "content": "clobber"}); err == nil || !strings.Contains(err.Error(), "FS_STALE") {
		t.Fatalf("stale write: %v", err)
	}
	if _, err := call("read", map[string]any{"file_path": path}); err != nil {
		t.Fatal(err)
	}
	if _, err := call("edit", map[string]any{"file_path": path, "old_string": "externally changed", "new_string": "after"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call("edit", map[string]any{"file_path": path, "old_string": "after", "new_string": "twice"}); err != nil {
		t.Fatalf("successful mutation did not refresh observation: %v", err)
	}
	tc.SessionID = "two"
	if _, err := call("write", map[string]any{"file_path": path, "content": "no"}); err == nil {
		t.Fatal("another session inherited observation")
	}
	if _, err := call("write", map[string]any{"file_path": "new", "content": "created"}); err != nil {
		t.Fatal(err)
	}
}

func TestOfficialReadStreamingWindowAndCaps(t *testing.T) {
	r := New()
	defer r.Close()
	tc := core.ToolContext{SessionID: "read", CWD: t.TempDir(), Permission: "read-only"}
	path := filepath.Join(tc.CWD, "large")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 17<<20)+"\r\nsecond\nthird\n"), 0600); err != nil {
		t.Fatal(err)
	}
	res, err := r.Execute(context.Background(), tc, "read", core.JSON(map[string]any{"file_path": path, "offset": 2, "limit": 1}))
	if err != nil {
		t.Fatal(err)
	}
	var value readValue
	if err = json.Unmarshal([]byte(res.Content), &value); err != nil {
		t.Fatal(err)
	}
	if value.TotalLines != 3 || len(value.Lines) != 1 || value.Lines[0].Number != 2 || value.Lines[0].Text != "second" {
		t.Fatalf("wrong streamed window: %+v", value)
	}
	for _, offset := range []int{0, 4} {
		if _, err = r.Execute(context.Background(), tc, "read", core.JSON(map[string]any{"file_path": path, "offset": offset})); err == nil {
			t.Fatalf("offset %d accepted", offset)
		}
	}
	res, err = r.Execute(context.Background(), tc, "read", core.JSON(map[string]any{"file_path": path, "limit": 1}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "line truncated to 2000 chars") {
		t.Fatal("giant line not bounded")
	}
	text, ok := RenderOfficialFileResult("read", res.Content)
	if !ok || !strings.Contains(text, "Use offset=2 to continue.") {
		t.Fatalf("missing continuation: %s", text)
	}
}

func TestOfficialReadAcceptsEquivalentIntegralJSONNumbers(t *testing.T) {
	r := New()
	defer r.Close()
	tc := toolContext(t)
	os.WriteFile(filepath.Join(tc.CWD, "text"), []byte("one\ntwo\n"), 0600)
	for _, raw := range []string{`{"file_path":"text","offset":1.0,"limit":1e0}`, `{"file_path":"text","offset":2e0,"limit":1.000}`} {
		if _, err := r.Execute(context.Background(), tc, "read", json.RawMessage(raw)); err != nil {
			t.Fatalf("valid integral number rejected: %s: %v", raw, err)
		}
	}
	for _, value := range []string{"0", "-1", "1.5", "null", "\"1\""} {
		raw := `{"file_path":"text","offset":` + value + `}`
		if _, err := r.Execute(context.Background(), tc, "read", json.RawMessage(raw)); err == nil {
			t.Fatalf("nonpositive/nonintegral offset accepted: %s", raw)
		}
	}
}
