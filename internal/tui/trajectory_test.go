package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"dsgo/internal/core"
)

func event(seq int64, turn, kind string, second int, v any) core.Event {
	return core.Event{Seq: seq, ID: "ev-" + kind, TurnID: turn, Type: kind, Time: time.Unix(1000+int64(second), 0).UTC(), Data: mustJSON(v)}
}
func fixtureEvents() []core.Event {
	first, last := time.Unix(1002, 0).UTC(), time.Unix(1004, 0).UTC()
	return []core.Event{
		event(1, "a", "turn/started", 0, map[string]any{"id": "a"}), event(2, "a", "user/message", 0, core.Message{Role: "user", Content: []core.Block{{Type: "text", Text: "Inspect repository"}, {Type: "image"}}}),
		event(3, "a", "step/started", 1, map[string]any{"step": 1, "model": "fixture"}), event(4, "a", "step/timing", 5, map[string]any{"step": 1, "started_at": time.Unix(1001, 0).UTC(), "completed_at": time.Unix(1005, 0).UTC(), "first_token_at": first, "last_token_at": last}),
		event(5, "a", "assistant/message", 5, core.Message{Role: "assistant", Content: []core.Block{{Type: "thinking", Thinking: "Inspect first"}, {Type: "text", Text: "Checking"}, {Type: "tool_use", ID: "code-1", Name: "run_code", Input: json.RawMessage(`{ "code": "return tools.read_file({path:'x'});\n" }`)}}}), event(6, "a", "step/usage", 5, core.Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 5}),
		event(7, "a", "tool/started", 6, map[string]any{"type": "tool_use", "id": "code-1", "name": "run_code", "input": map[string]any{"code": "return tools.read_file({path:'x'});\n"}, "input_raw": `{ "code": "return tools.read_file({path:'x'});\n" }`}),
		event(8, "a", "tool/subcall", 6, map[string]any{"id": 1, "parent_id": "code-1", "call_id": "code-1/1", "name": "read_file", "input": map[string]any{"path": "x"}}), event(9, "a", "tool/subcall", 6, map[string]any{"id": 2, "parent_id": "code-1", "call_id": "code-1/2", "name": "read_file", "input": map[string]any{"path": "missing"}}),
		event(10, "a", "tool/subresult", 7, map[string]any{"id": 2, "parent_id": "code-1", "call_id": "code-1/2", "name": "read_file", "result": core.ToolResult{}, "error": "missing"}), event(11, "a", "tool/subresult", 8, map[string]any{"id": 1, "parent_id": "code-1", "call_id": "code-1/1", "name": "read_file", "result": core.ToolResult{Content: "data"}}), event(12, "a", "tool/finished", 9, map[string]any{"id": "code-1", "name": "run_code", "result": core.ToolResult{Content: `"data"`}}),
		event(13, "a", "step/started", 10, map[string]any{"step": 2, "model": "fixture"}), event(14, "a", "assistant/message", 11, core.Message{Role: "assistant", Content: []core.Block{{Type: "text", Text: "Done"}}}), event(15, "a", "step/usage", 11, core.Usage{InputTokens: 150, OutputTokens: 10}), event(16, "a", "turn/finished", 12, map[string]any{"status": "completed"}),
		event(17, "", "context/compacted", 13, map[string]any{"messages": []core.Message{}}), event(18, "b", "turn/started", 14, map[string]any{"id": "b"}), event(19, "b", "step/started", 15, map[string]any{"step": 1, "model": "fixture"}),
	}
}
func TestTrajectoryPairsPTCTreeUsageAndRecordedTiming(t *testing.T) {
	p := Project(fixtureEvents(), true)
	if p.Stats.Turns != 2 || p.Stats.Requests != 3 || p.Stats.Tools != 3 || p.Stats.Subtools != 2 || p.Stats.FailedTools != 1 || p.Stats.Usage.InputTokens != 250 || p.Stats.Usage.OutputTokens != 30 {
		t.Fatalf("stats %+v", p.Stats)
	}
	if p.Turns[0].Title != "Inspect repository" || p.Turns[0].Status != "completed" || p.Turns[1].Status != "running" {
		t.Fatalf("turns %+v", p.Turns)
	}
	var request, subtool, code *Record
	for i := range p.Records {
		r := &p.Records[i]
		if r.Kind == "request" && r.Turn == 1 && r.Step == 1 {
			request = r
		}
		if r.ID == "code-1/2" {
			subtool = r
		}
		if r.ID == "code-1" {
			code = r
		}
	}
	if request == nil || subtool == nil || code == nil {
		t.Fatal("missing records")
	}
	if d, ok := request.TTFT(); !ok || d != time.Second {
		t.Fatalf("TTFT %v %v", d, ok)
	}
	if rate, ok := request.TokensPerSecond(); !ok || rate != 10 {
		t.Fatalf("rate %v %v", rate, ok)
	}
	if subtool.ParentID != "code-1" || subtool.Status != "failed" || subtool.Error != "missing" {
		t.Fatalf("subtool %+v", subtool)
	}
	if string(code.Input) != `{ "code": "return tools.read_file({path:'x'});\n" }` {
		t.Fatalf("original args changed: %s", code.Input)
	}
	if !strings.Contains(strings.Join(Inspector(*code, "code"), "\n"), "   2  ") {
		t.Fatal("code trailing newline lost")
	}
	if p.Records[len(p.Records)-2].Kind != "compaction" {
		t.Fatalf("standalone compaction order %+v", p.Records)
	}
}
func TestTrajectoryPendingAndInterruptionDoNotInventDurations(t *testing.T) {
	events := fixtureEvents()
	p := Project(events, false)
	tail := p.Records[len(p.Records)-1]
	if tail.Status != "interrupted" || p.Turns[1].Status != "interrupted" {
		t.Fatalf("tail %+v", tail)
	}
	if _, ok := tail.Duration(); ok {
		t.Fatal("invented request duration")
	}
	if _, ok := tail.TTFT(); ok {
		t.Fatal("invented TTFT")
	}
	if _, ok := tail.TokensPerSecond(); ok {
		t.Fatal("invented rate")
	}
	events = append(events, event(20, "b", "assistant/attempt", 16, map[string]any{"response": core.Response{Content: []core.Block{{Type: "text", Text: "partial"}}}, "error": "network failed"}), event(21, "b", "turn/finished", 17, map[string]any{"status": "failed", "error": "network failed"}))
	p = Project(events, false)
	last := p.Records[len(p.Records)-1]
	if last.Status != "failed" || last.Text != "partial" || last.Error != "network failed" {
		t.Fatalf("failed attempt %+v", last)
	}
	if p.Stats.FailedRequests != 1 {
		t.Fatalf("fail count %+v", p.Stats)
	}
}
func TestTrajectoryRecoveryAndLegacySubcallPairing(t *testing.T) {
	events := []core.Event{event(1, "a", "turn/started", 0, nil), event(2, "a", "tool/started", 1, core.Block{ID: "parent", Name: "run_code", Input: json.RawMessage(`{}`)}), event(3, "a", "tool/subcall", 1, map[string]any{"id": 1, "name": "read_file", "input": map[string]any{}}), event(4, "a", "tool/subresult", 2, map[string]any{"id": 1, "name": "read_file", "result": core.ToolResult{Content: "ok"}}), event(5, "b", "turn/started", 10, nil), event(6, "b", "tool/result", 10, core.Message{Content: []core.Block{{Type: "tool_result", ToolUseID: "parent", Content: "Previous process ended", IsError: true}}})}
	p := Project(events, false)
	if p.Records[0].Status != "interrupted" || p.Records[0].End != nil {
		t.Fatalf("recovery invented elapsed %+v", p.Records[0])
	}
	if p.Records[1].ParentID != "parent" || p.Records[1].Status != "completed" {
		t.Fatalf("legacy subtool %+v", p.Records[1])
	}
}
func TestSafeTerminalControlsAndCellWrapping(t *testing.T) {
	unsafe := "hello\x1b[2J\x1b]52;c;SECRET\a世界\x1bPpayload\x1b\\!\r\b\u009b31mred\x1b[0m"
	got := Safe(unsafe)
	if got != "hello世界!red" {
		t.Fatalf("unsafe terminal data %q", got)
	}
	wrapped := Wrap([]string{"中a中b", "x\ny"}, 4)
	if strings.Join(wrapped, "|") != "中a|中b|x|y" {
		t.Fatalf("wrap %#v", wrapped)
	}
	if clipCells("中ab", 3) != "中a" {
		t.Fatal("wrong terminal width")
	}
}
func TestPagerNavigationSearchAndFragmentedKeys(t *testing.T) {
	p := pager{data: Project(fixtureEvents(), false), height: 10, width: 80, follow: true, tab: "summary"}
	p.selected = len(p.rows()) - 1
	p.handle("home")
	if p.selected != 0 || p.follow {
		t.Fatal("home")
	}
	p.handle("/")
	for _, c := range "missing" {
		p.handle(string(c))
	}
	p.handle("enter")
	if p.rows()[p.selected].Error != "missing" {
		t.Fatal("search failed")
	}
	p.handle("enter")
	p.handle("r")
	if !p.detail || p.tab != "raw" {
		t.Fatal("inspector")
	}
	if keys := p.decodeKeys([]byte("\x1b[")); len(keys) != 0 {
		t.Fatalf("partial arrow %v", keys)
	}
	keys := p.decodeKeys([]byte("A\x1b[6~中"))
	if strings.Join(keys, ",") != "up,page-down,中" {
		t.Fatalf("keys %#v", keys)
	}
	if !p.handle("q") {
		t.Fatal("quit")
	}
}

func TestTrajectoryInputBatchKeepsCapturedContextAdjacent(t *testing.T) {
	direct := core.TextMessage("user", "Compare @source")
	captured := core.TextMessage("user", "immutable untrusted snapshot")
	captured.Source = map[string]any{"kind": "session-reference"}
	events := []core.Event{
		{ID: "turn", Seq: 1, TurnID: "one", Type: "turn/started", Time: time.Now(), Data: core.JSON(map[string]any{})},
		{ID: "input", Seq: 2, TurnID: "one", Type: "user/input", Time: time.Now(), Data: core.JSON(core.InputBatch{Version: 1, Message: direct, Contexts: []core.Message{captured}})},
	}
	view := Project(events, true)
	if len(view.Records) != 2 || view.Records[0].Kind != "user" || view.Records[1].Kind != "context" || view.Records[1].Text != captured.Content[0].Text {
		t.Fatalf("batch projection %+v", view.Records)
	}
	if view.Records[0].ID == view.Records[1].ID || len(view.Turns) != 1 || view.Turns[0].Title != "Compare @source" {
		t.Fatalf("batch identities/title %+v", view)
	}
}
