package cli

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"dsgo/internal/agent"
	"dsgo/internal/core"
	"dsgo/internal/state"
	"dsgo/internal/tools"
)

func TestScheduledRecoveryContinuesAcceptedPromptWithoutDuplicatingIt(t *testing.T) {
	s, _ := state.Open(t.TempDir())
	session, _ := s.Create(t.TempDir(), "scheduled")
	// This is the exact no-Source record written by the original Go daemon.
	input := core.TextMessage("user", "Scheduled reminder (2026-10-27T09:00:00Z):\nscheduled work")
	input.DeliveryID = "0123456789abcdef0123"
	_, _ = s.Append(session.ID, "crashed", "turn/started", map[string]any{})
	_, _ = s.Append(session.ID, "crashed", "user/message", input)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &App{store: s, ctx: ctx, out: io.Discard}
	called := make(chan struct{}, 2)
	a.engine = agent.New(s, goalModel(func(ctx context.Context, r core.Request, emit func(core.Delta)) (core.Response, error) {
		if len(r.Messages) != 1 || r.Messages[0].DeliveryID != "0123456789abcdef0123" {
			t.Error("recovery inserted another prompt")
		}
		called <- struct{}{}
		return core.Response{Content: []core.Block{{Type: "text", Text: "recovered"}}}, nil
	}), &toolsExecutorStub{}, agent.Options{})
	defer a.engine.Close()
	if err := a.recoverScheduled(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("cold scheduled session was not awakened")
	}
	a.background.Wait()
	if err := a.recoverScheduled(ctx); err != nil {
		t.Fatal(err)
	}
	a.background.Wait()
	if len(called) != 0 {
		t.Fatal("settled recovery was run twice")
	}
	events, _ := s.Events(session.ID)
	users := 0
	for _, ev := range events {
		if ev.Type == "user/message" {
			users++
		}
	}
	if users != 1 {
		t.Fatalf("duplicate scheduled prompts=%d", users)
	}
}

type toolsExecutorStub struct{}

func (*toolsExecutorStub) Specs() []core.ToolSpec { return nil }
func (*toolsExecutorStub) Execute(context.Context, core.ToolContext, string, json.RawMessage) (core.ToolResult, error) {
	panic("no tools")
}

func TestPlanModeIsDurableAndForkKeepsPolicy(t *testing.T) {
	s, _ := state.Open(t.TempDir())
	session, _ := s.Create(t.TempDir(), "plan")
	a := &App{store: s, session: session, registry: tools.New(), cfg: state.DefaultConfig()}
	a.current.Store(session.ID)
	a.manager = newAgents(a)
	defer a.registry.Close()
	defer a.manager.close()
	if err := a.setPlan(session.ID, true); err != nil {
		t.Fatal(err)
	}
	saved, _ := s.Get(session.ID)
	if !saved.Plan {
		t.Fatal("plan mode lost after restart")
	}
	fork, err := s.Fork(session.ID, 0)
	if err != nil || !fork.Plan {
		t.Fatalf("fork lost plan policy: %v", err)
	}
	if !a.planFor(fork.ID) {
		t.Fatal("background session read current session's unrelated plan mode")
	}
	if err := a.setPlan(session.ID, false); err != nil {
		t.Fatal(err)
	}
	if a.planFor(session.ID) || !a.planFor(fork.ID) {
		t.Fatal("session plan modes leaked into each other")
	}
}
