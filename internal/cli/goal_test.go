package cli

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"dsgo/internal/agent"
	"dsgo/internal/core"
	"dsgo/internal/schedule"
	"dsgo/internal/state"
	"dsgo/internal/tools"
)

type goalModel func(context.Context, core.Request, func(core.Delta)) (core.Response, error)

func (f goalModel) Stream(ctx context.Context, r core.Request, emit func(core.Delta)) (core.Response, error) {
	return f(ctx, r, emit)
}

func TestGoalRequiresUserCreationAndDrivesUntilCompletion(t *testing.T) {
	s, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.Create(t.TempDir(), "goal")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &App{store: s, session: session, ctx: ctx, cancel: cancel, out: io.Discard, err: io.Discard, cfg: state.DefaultConfig(), unattended: true, registry: tools.New()}
	defer a.registry.Close()
	a.current.Store(session.ID)
	a.schedules, _ = schedule.Open(s.Root())
	a.manager = newAgents(a)
	defer a.manager.close()
	if _, err = a.updateGoal(ctx, core.ToolContext{SessionID: session.ID}, core.JSON(map[string]any{"objective": "unrequested", "status": "active"})); err == nil {
		t.Fatal("model created an unrequested goal")
	}
	calls := 0
	a.engine = agent.New(s, goalModel(func(ctx context.Context, r core.Request, emit func(core.Delta)) (core.Response, error) {
		calls++
		switch calls {
		case 1:
			return core.Response{Content: []core.Block{{Type: "text", Text: "First phase complete; still working."}}, Usage: core.Usage{InputTokens: 5, OutputTokens: 5}}, nil
		case 2:
			return core.Response{Content: []core.Block{{Type: "tool_use", ID: "goal-complete", Name: "update_goal", Input: json.RawMessage(`{"objective":"finish the fixture","status":"complete"}`)}}}, nil
		default:
			return core.Response{Content: []core.Block{{Type: "text", Text: "Done with evidence."}}}, nil
		}
	}), a, agent.Options{Model: "fixture"})
	defer a.engine.Close()
	if err = a.registerTools(); err != nil {
		t.Fatal(err)
	}
	if err = a.goalCommand("finish the fixture"); err != nil {
		t.Fatal(err)
	}
	g, tokens, err := a.goalState(session.ID)
	if err != nil || g.Status != "complete" || calls != 3 || tokens != 10 {
		t.Fatalf("goal=%#v tokens=%d calls=%d err=%v", g, tokens, calls, err)
	}
}

func TestGoalBudgetPausesWithoutLosingObjective(t *testing.T) {
	s, _ := state.Open(t.TempDir())
	session, _ := s.Create(t.TempDir(), "budget")
	a := &App{store: s, session: session, out: io.Discard}
	event, _ := s.Append(session.ID, "", "goal/updated", goal{Objective: "keep working", Status: "active", TokenBudget: 10})
	_, _ = s.Append(session.ID, "", "step/usage", core.Usage{InputTokens: 6, OutputTokens: 5})
	prompt, err := a.goalContinuation()
	if err != nil || prompt != "" {
		t.Fatalf("budget not stopped: %q %v", prompt, err)
	}
	g, used, _ := a.goalState(session.ID)
	if g.Status != "paused" || g.Objective != "keep working" || g.StartedSeq != event.Seq || used != 11 {
		t.Fatalf("lost goal state: %#v %d", g, used)
	}
}
