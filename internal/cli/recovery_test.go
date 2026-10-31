package cli

import (
	"context"
	"encoding/json"
	"testing"

	"dsgo/internal/core"
	"dsgo/internal/state"
	"dsgo/internal/tools"
)

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
