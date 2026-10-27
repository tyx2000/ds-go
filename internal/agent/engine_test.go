package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"dsgo/internal/core"
	"dsgo/internal/state"
)

type fakeModel struct {
	f func(context.Context, core.Request, func(core.Delta)) (core.Response, error)
}

func (f fakeModel) Stream(ctx context.Context, r core.Request, emit func(core.Delta)) (core.Response, error) {
	return f.f(ctx, r, emit)
}

type fakeTools struct {
	calls int
	f     func(context.Context) error
}

func (f *fakeTools) Specs() []core.ToolSpec {
	return []core.ToolSpec{{Name: "test", InputSchema: json.RawMessage(`{"type":"object"}`)}}
}
func (f *fakeTools) Execute(ctx context.Context, tc core.ToolContext, name string, input json.RawMessage) (core.ToolResult, error) {
	f.calls++
	if f.f != nil {
		if err := f.f(ctx); err != nil {
			return core.ToolResult{}, err
		}
	}
	if tc.Emit != nil {
		tc.Emit("test/audit", map[string]any{"name": name})
	}
	return core.ToolResult{Content: "tool output"}, nil
}

func setup(t *testing.T) (*state.Store, core.Session) {
	t.Helper()
	s, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.Create(t.TempDir(), "New session")
	if err != nil {
		t.Fatal(err)
	}
	session.Model = "test-model"
	session.Permission = "workspace-write"
	if err = s.Update(session); err != nil {
		t.Fatal(err)
	}
	return s, session
}

func TestToolLoopPersistsReplayAndAudit(t *testing.T) {
	s, session := setup(t)
	calls := 0
	tool := &fakeTools{}
	model := fakeModel{func(ctx context.Context, r core.Request, emit func(core.Delta)) (core.Response, error) {
		calls++
		if r.SessionID != session.ID {
			t.Error("missing attribution")
		}
		if calls == 1 {
			return core.Response{Content: []core.Block{{Type: "thinking", Thinking: "reason", Signature: "sig"}, {Type: "tool_use", ID: "call-1", Name: "test", Input: json.RawMessage(`{}`)}}, StopReason: "tool_use"}, nil
		}
		if len(r.Messages) != 3 || r.Messages[1].ReplayModel != "test-model" || r.Messages[2].Content[0].ToolUseID != "call-1" {
			t.Errorf("bad replay: %#v", r.Messages)
		}
		emit(core.Delta{Type: "text", Text: "done"})
		return core.Response{Content: []core.Block{{Type: "text", Text: "done"}}, StopReason: "end_turn"}, nil
	}}
	e := New(s, model, tool, Options{Model: "test-model"})
	if err := e.Run(context.Background(), session.ID, core.TextMessage("user", "edit")); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || tool.calls != 1 {
		t.Fatalf("model=%d tools=%d", calls, tool.calls)
	}
	events, err := s.Events(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, v := range events {
		seen[v.Type] = true
	}
	for _, kind := range []string{"turn/started", "user/message", "assistant/message", "tool/result", "test/audit", "turn/finished"} {
		if !seen[kind] {
			t.Errorf("missing %s", kind)
		}
	}
}

func TestCancelledAttemptIsAuditableButNotReplayed(t *testing.T) {
	s, session := setup(t)
	model := fakeModel{func(ctx context.Context, r core.Request, emit func(core.Delta)) (core.Response, error) {
		return core.Response{Content: []core.Block{{Type: "text", Text: "partial"}}}, context.Canceled
	}}
	e := New(s, model, &fakeTools{}, Options{})
	err := e.Run(context.Background(), session.ID, core.TextMessage("user", "start"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	messages, _ := s.Messages(session.ID)
	if len(messages) != 1 {
		t.Fatalf("attempt entered replay: %#v", messages)
	}
	events, _ := s.Events(session.ID)
	found := false
	for _, v := range events {
		if v.Type == "assistant/attempt" {
			found = true
		}
	}
	if !found {
		t.Fatal("attempt lost")
	}
}

func TestCancelClosesAllToolCallsAndRejectsConcurrentTurn(t *testing.T) {
	s, session := setup(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	model := fakeModel{func(ctx context.Context, r core.Request, emit func(core.Delta)) (core.Response, error) {
		return core.Response{Content: []core.Block{{Type: "tool_use", ID: "a", Name: "test", Input: json.RawMessage(`{}`)}, {Type: "tool_use", ID: "b", Name: "test", Input: json.RawMessage(`{}`)}}}, nil
	}}
	tool := &fakeTools{f: func(ctx context.Context) error {
		once.Do(func() { close(started) })
		<-ctx.Done()
		close(release)
		return ctx.Err()
	}}
	e := New(s, model, tool, Options{})
	done := make(chan error, 1)
	go func() { done <- e.Run(context.Background(), session.ID, core.TextMessage("user", "start")) }()
	<-started
	if err := e.Run(context.Background(), session.ID, core.TextMessage("user", "second")); err == nil {
		t.Fatal("concurrent turn accepted")
	}
	e.Cancel(session.ID)
	<-release
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	messages, _ := s.Messages(session.ID)
	last := messages[len(messages)-1]
	if len(last.Content) != 2 || last.Content[1].ToolUseID != "b" || !last.Content[1].IsError {
		t.Fatalf("dangling tool call: %#v", last)
	}
}

func TestCrashRecoveryRepairsToolBoundaryBeforeNextUser(t *testing.T) {
	s, session := setup(t)
	_, _ = s.Append(session.ID, "old", "user/message", core.TextMessage("user", "old"))
	_, _ = s.Append(session.ID, "old", "assistant/message", core.Message{Role: "assistant", Content: []core.Block{{Type: "tool_use", ID: "dangling", Name: "test", Input: json.RawMessage(`{}`)}}})
	model := fakeModel{func(ctx context.Context, r core.Request, emit func(core.Delta)) (core.Response, error) {
		if len(r.Messages) != 4 || r.Messages[2].Content[0].ToolUseID != "dangling" || r.Messages[3].Content[0].Text != "new" {
			t.Fatalf("invalid repaired context: %#v", r.Messages)
		}
		return core.Response{Content: []core.Block{{Type: "text", Text: "done"}}}, nil
	}}
	e := New(s, model, &fakeTools{}, Options{})
	if err := e.Run(context.Background(), session.ID, core.TextMessage("user", "new")); err != nil {
		t.Fatal(err)
	}
}

func TestDurableDeliveryDedupSurvivesCompactionAndRestart(t *testing.T) {
	s, session := setup(t)
	input := core.TextMessage("user", "scheduled")
	input.DeliveryID = "delivery-1"
	_, _ = s.Append(session.ID, "old", "user/message", input)
	_, _ = s.Append(session.ID, "old", "context/compacted", map[string]any{"messages": []core.Message{core.TextMessage("user", "summary")}})
	called := false
	e := New(s, fakeModel{func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		called = true
		return core.Response{}, nil
	}}, &fakeTools{}, Options{})
	if err := e.Run(context.Background(), session.ID, input); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("replayed an already admitted scheduled occurrence after compaction")
	}
	events, _ := s.Events(session.ID)
	if len(events) != 2 {
		t.Fatal("dedup appended a second turn")
	}
}

func TestPreCancelledTurnAndClosedEngineHaveNoSideEffects(t *testing.T) {
	s, session := setup(t)
	e := New(s, fakeModel{}, &fakeTools{}, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.Run(ctx, session.ID, core.TextMessage("user", "cancelled")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	e.Close()
	if err := e.Run(context.Background(), session.ID, core.TextMessage("user", "closed")); err == nil {
		t.Fatal("closed engine accepted work")
	}
	events, _ := s.Events(session.ID)
	if len(events) != 0 {
		t.Fatal("cancelled work was admitted")
	}
}
