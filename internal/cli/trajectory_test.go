package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"dsgo/internal/core"
)

func TestTrajectoryCommandPlainSelectedTurnAndStatistics(t *testing.T) {
	a, parent := lifecycleApp(t, lifecycleModel(nil))
	a.session = parent
	var out bytes.Buffer
	a.out = &out
	for _, record := range []struct {
		kind string
		data any
	}{{"turn/started", map[string]any{"id": "turn"}}, {"user/message", core.TextMessage("user", "Inspect\x1b]52;c;secret\a source")}, {"step/started", map[string]any{"step": 1, "model": "fixture"}}, {"assistant/message", core.TextMessage("assistant", "answer")}, {"step/usage", core.Usage{InputTokens: 20, OutputTokens: 5}}, {"tool/started", core.Block{ID: "read", Name: "read_file", Input: json.RawMessage(`{"path":"a.go"}`)}}, {"tool/finished", map[string]any{"id": "read", "name": "read_file", "result": core.ToolResult{Content: "contents"}}}, {"turn/finished", map[string]any{"status": "completed"}}} {
		if _, err := a.store.Append(parent.ID, "turn", record.kind, record.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.trajectoryCommand("--plain"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Turn outline") || strings.Contains(out.String(), "\x1b") || strings.Contains(out.String(), "secret") {
		t.Fatalf("plain output %q", out.String())
	}
	out.Reset()
	if err := a.trajectoryCommand("1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "a.go") || !strings.Contains(out.String(), "contents") || !strings.Contains(out.String(), "TTFT: unavailable") {
		t.Fatalf("selected detail %s", out.String())
	}
	out.Reset()
	if err := a.trajectoryCommand("stats"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "input 20") || !strings.Contains(out.String(), "output 5") {
		t.Fatalf("stats %s", out.String())
	}
	if err := a.trajectoryCommand("turn 2"); err == nil {
		t.Fatal("unknown turn accepted")
	}
}
func TestTrajectoryCommandJSONIsStructuredAndReadOnly(t *testing.T) {
	a, parent := lifecycleApp(t, lifecycleModel(nil))
	a.session = parent
	a.flags.json = true
	var out bytes.Buffer
	a.out = &out
	if _, err := a.store.Append(parent.ID, "turn", "turn/started", map[string]any{"id": "turn"}); err != nil {
		t.Fatal(err)
	}
	before, _ := a.store.Events(parent.ID)
	if err := a.trajectoryCommand(""); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	turns := payload["turns"].([]any)
	if turns[0].(map[string]any)["status"] != "interrupted" {
		t.Fatalf("JSON %+v", payload)
	}
	after, _ := a.store.Events(parent.ID)
	if len(before) != len(after) {
		t.Fatal("trajectory modified session")
	}
}
