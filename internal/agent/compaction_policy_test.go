package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"dsgo/internal/core"
	"dsgo/internal/state"
)

func TestCompactionSpecFollowsPinnedDefaults(t *testing.T) {
	for _, c := range []struct{ window, output, threshold, retain int }{
		{1000000, 16384, 800000, 157378}, // W*0.8 is the binding limit
		{100000, 16384, 18080, 13378},    // W-O-65536 is the binding limit
		{4000, 100, 3200, 624},           // a window below the headroom uses the ratio
	} {
		threshold, retain, ok := compactionSpec(c.window, c.output)
		if !ok || threshold != c.threshold || retain != c.retain {
			t.Fatalf("W=%d O=%d: got %d/%d want %d/%d", c.window, c.output, threshold, retain, c.threshold, c.retain)
		}
	}
	if _, _, ok := compactionSpec(0, 0); ok {
		t.Fatal("unknown window produced a threshold")
	}
}

func TestCompactionCutKeepsToolPairsAndInputGroups(t *testing.T) {
	text := func(role, s string) core.ContextNode { return core.ContextNode{Message: core.TextMessage(role, s)} }
	call := core.ContextNode{Message: core.Message{Role: "assistant", Content: []core.Block{{Type: "tool_use", ID: "c1", Name: "test"}}}}
	result := core.ContextNode{Message: core.Message{Role: "user", Content: []core.Block{{Type: "tool_result", ToolUseID: "c1", Content: "ok"}}}}
	nodes := []core.ContextNode{text("user", "old"), text("assistant", "old answer"), call, result, text("assistant", strings.Repeat("x", 400))}
	// The budget is met at the last message; a cut between a call and its
	// result is moved earlier.
	if cut, _ := compactionCut(nodes, 1); cut != 4 {
		t.Fatalf("cut after the balanced pair: %d", cut)
	}
	if cut, _ := compactionCut(nodes[:4], 1); cut != 2 {
		t.Fatalf("cut inside a tool pair: %d", cut)
	}
	grouped := []core.ContextNode{text("user", "old"), {InputGroup: 7, Message: core.TextMessage("user", "input")}, {InputGroup: 7, Message: core.TextMessage("user", "captured context")}}
	if cut, _ := compactionCut(grouped, 1); cut != 1 {
		t.Fatalf("cut inside an input group: %d", cut)
	}
}

func TestContextOverflowRecognizesProviderDialects(t *testing.T) {
	for _, message := range []string{
		"This model's maximum context length is 131072 tokens",
		"error: context_length_exceeded",
		"Prompt is too long for the model's context window",
		"input exceeds the model's context window",
	} {
		if !isContextOverflow(errors.New(message)) {
			t.Fatalf("missed overflow: %q", message)
		}
	}
	for _, message := range []string{"rate limit reached", "invalid api key"} {
		if isContextOverflow(errors.New(message)) {
			t.Fatalf("false overflow: %q", message)
		}
	}
}

// longHistory appends completed turns of roughly tokens estimated tokens.
func longHistory(t *testing.T, store *state.Store, id string, turns, chars int) {
	t.Helper()
	for i := 0; i < turns; i++ {
		turn := ID()
		for _, event := range []struct {
			kind string
			data any
		}{
			{"turn/started", map[string]any{"id": turn}},
			{"user/message", core.TextMessage("user", strings.Repeat("u", chars))},
			{"assistant/message", core.TextMessage("assistant", strings.Repeat("a", chars))},
			{"turn/finished", map[string]any{"status": "completed"}},
		} {
			if _, err := store.Append(id, turn, event.kind, event.data); err != nil {
				t.Fatal(err)
			}
		}
	}
}

type recordedRequests struct {
	mu       sync.Mutex
	agent    []core.Request
	compacts int
}

func (r *recordedRequests) model(agentErr func(int) error) fakeModel {
	return fakeModel{func(ctx context.Context, req core.Request, emit func(core.Delta)) (core.Response, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if req.Purpose == "compaction" {
			r.compacts++
			return core.Response{Content: []core.Block{{Type: "text", Text: "SUMMARY"}}}, nil
		}
		r.agent = append(r.agent, req)
		if agentErr != nil {
			if err := agentErr(len(r.agent)); err != nil {
				return core.Response{}, err
			}
		}
		return core.Response{Content: []core.Block{{Type: "text", Text: "done"}}}, nil
	}}
}

func TestPressureCompactsOldestHistoryAndKeepsRecentTail(t *testing.T) {
	store, session := setup(t)
	// Six turns of about 1000 tokens each against a 3200-token threshold.
	longHistory(t, store, session.ID, 6, 2000)
	requests := &recordedRequests{}
	e := New(store, requests.model(nil), &fakeTools{}, Options{Model: "test-model", ContextTokens: 4000, MaxTokens: 100})
	if err := e.Run(context.Background(), session.ID, core.TextMessage("user", "next")); err != nil {
		t.Fatal(err)
	}
	if requests.compacts == 0 {
		t.Fatal("pressure did not condense history")
	}
	sent := requests.agent[0].Messages
	if !strings.Contains(sent[0].Content[0].Text, "SUMMARY") || sent[len(sent)-1].Content[0].Text != "next" {
		t.Fatalf("condensed request: first=%q last=%q", sent[0].Content[0].Text, sent[len(sent)-1].Content[0].Text)
	}
	if total, _ := e.measuredTokens(context.Background(), session.ID, requests.agent[0]); total >= 3200 {
		t.Fatalf("request still over the threshold: %d", total)
	}
}

func TestPressureBelowThresholdAfterPruningSkipsSummary(t *testing.T) {
	store, session := setup(t)
	turn := ID()
	for _, event := range []struct {
		kind string
		data any
	}{
		{"turn/started", map[string]any{"id": turn}},
		{"user/message", core.TextMessage("user", "read the log")},
		{"assistant/message", core.Message{Role: "assistant", Content: []core.Block{{Type: "tool_use", ID: "c1", Name: "test", Input: []byte(`{}`)}}}},
		{"tool/result", core.Message{Role: "user", Content: []core.Block{{Type: "tool_result", ToolUseID: "c1", Content: strings.Repeat("log line\n", 4000)}}}},
		{"assistant/message", core.TextMessage("assistant", "read it")},
		{"turn/finished", map[string]any{"status": "completed"}},
	} {
		if _, err := store.Append(session.ID, turn, event.kind, event.data); err != nil {
			t.Fatal(err)
		}
	}
	requests := &recordedRequests{}
	e := New(store, requests.model(nil), &fakeTools{}, Options{Model: "test-model", ContextTokens: 10000, MaxTokens: 100})
	if err := e.Run(context.Background(), session.ID, core.TextMessage("user", "next")); err != nil {
		t.Fatal(err)
	}
	if requests.compacts != 0 {
		t.Fatal("summarized although pruning relieved the pressure")
	}
	sent := requests.agent[0].Messages
	if sent[2].Content[0].Type != "tool_result" {
		t.Fatalf("unexpected request shape: %+v", sent[2])
	}
	// Whatever form the content takes, the request carries the bounded text.
	content := string(core.JSON(sent[2].Content[0].Content))
	if !strings.Contains(content, "tool result middle pruned") || len(content) > 9000 {
		t.Fatalf("tool result was not pruned: %d bytes", len(content))
	}
	// The complete original stays in the durable log.
	events, _ := store.Events(session.ID)
	original := false
	for _, event := range events {
		original = original || (event.Type == "tool/result" && len(event.Data) > 30000)
	}
	if !original {
		t.Fatal("original tool result missing from the log")
	}
}

func TestOverflowCondensesAndRetriesOnce(t *testing.T) {
	store, session := setup(t)
	longHistory(t, store, session.ID, 4, 200)
	overflow := errors.New("This model's maximum context length is 4096 tokens")
	requests := &recordedRequests{}
	e := New(store, requests.model(func(n int) error {
		if n == 1 {
			return overflow
		}
		return nil
	}), &fakeTools{}, Options{Model: "test-model", MaxTokens: 100})
	if err := e.Run(context.Background(), session.ID, core.TextMessage("user", "next")); err != nil {
		t.Fatalf("recovered overflow failed: %v", err)
	}
	if requests.compacts != 1 || len(requests.agent) != 2 || !strings.Contains(requests.agent[1].Messages[0].Content[0].Text, "SUMMARY") {
		t.Fatalf("compactions=%d agent requests=%d", requests.compacts, len(requests.agent))
	}
	// A persistent overflow is retried once, then reported.
	store, session = setup(t)
	longHistory(t, store, session.ID, 4, 200)
	requests = &recordedRequests{}
	e = New(store, requests.model(func(int) error { return overflow }), &fakeTools{}, Options{Model: "test-model", MaxTokens: 100})
	if err := e.Run(context.Background(), session.ID, core.TextMessage("user", "next")); !errors.Is(err, overflow) || len(requests.agent) != 2 {
		t.Fatalf("persistent overflow: err=%v requests=%d", err, len(requests.agent))
	}
}
