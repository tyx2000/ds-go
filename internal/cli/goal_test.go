package cli

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dsgo/internal/agent"
	"dsgo/internal/core"
	"dsgo/internal/state"
	"dsgo/internal/tools"
)

type goalModel func(context.Context, core.Request, func(core.Delta)) (core.Response, error)

func (f goalModel) Stream(ctx context.Context, request core.Request, emit func(core.Delta)) (core.Response, error) {
	return f(ctx, request, emit)
}
func goalFixture(t *testing.T, model core.Model) *App {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(t.TempDir(), "goal")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &App{store: store, session: session, ctx: ctx, cancel: cancel, out: io.Discard, err: io.Discard, cfg: state.DefaultConfig(), unattended: true, registry: tools.New()}
	a.current.Store(session.ID)
	a.manager = newAgents(a)
	a.engine = agent.New(store, model, a, agent.Options{Model: "fixture", CommitPreparedInput: a.commitGoalInput, BeforeModel: a.beforeGoalModel})
	a.engine.Notify = a.notice
	if err := a.registerTools(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); a.closeGoals(); a.manager.close(); a.engine.Close(); a.registry.Close() })
	return a
}
func waitGoal(t *testing.T, a *App, phase string) *state.GoalView {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		view, err := a.goalService().Get(a.session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if view != nil && view.Phase == phase && !a.engine.Busy(a.session.ID) {
			return view
		}
		select {
		case <-deadline.C:
			t.Fatalf("goal did not reach %s: %+v", phase, view)
		case <-ticker.C:
		}
	}
}
func goalUpdateCall(view *state.GoalView, action string, extra map[string]any) core.Response {
	args := map[string]any{"goal_id": view.ID, "revision": view.Revision, "action": action}
	for key, value := range extra {
		args[key] = value
	}
	return core.Response{Content: []core.Block{{Type: "tool_use", ID: "goal-update", Name: "update_goal", Input: core.JSON(args)}}}
}

func TestGoalRoundCapIsAdmittedRoundsAndSurvivesRestartDisarmed(t *testing.T) {
	var calls atomic.Int32
	a := goalFixture(t, goalModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		calls.Add(1)
		return core.Response{Content: []core.Block{{Type: "text", Text: "progress"}}, Usage: core.Usage{InputTokens: 999, OutputTokens: 999}}, nil
	}))
	if err := a.goalCommand(`{"objective":"finish fixture","max_goal_rounds":2}`); err != nil {
		t.Fatal(err)
	}
	view := waitGoal(t, a, "blocked")
	if view.RoundsStarted != 2 || view.BlockedReason == nil || view.BlockedReason.Code != "round-limit" || calls.Load() != 2 {
		t.Fatalf("round policy: %+v calls=%d", view, calls.Load())
	}
	restarted := state.NewGoals(a.store)
	reloaded, err := restarted.Get(a.session.ID)
	if err != nil || reloaded.Activation != "disarmed" || reloaded.RoundsStarted != 2 {
		t.Fatal(reloaded, err)
	}
	events, err := a.store.Events(a.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == "goal/updated" {
			t.Fatal("new goal used token-budget legacy record")
		}
	}
}

func TestGoalModelCreatesFromDirectHumanAndAutonomousCompletionWritesWrapup(t *testing.T) {
	var a *App
	var calls atomic.Int32
	a = goalFixture(t, goalModel(func(_ context.Context, request core.Request, _ func(core.Delta)) (core.Response, error) {
		switch calls.Add(1) {
		case 1:
			return core.Response{Content: []core.Block{{Type: "tool_use", ID: "create", Name: "create_goal", Input: core.JSON(map[string]any{"objective": "complete delegated fixture", "max_goal_rounds": 4})}}}, nil
		case 2:
			return core.Response{Content: []core.Block{{Type: "text", Text: "initial work done"}}}, nil
		case 3:
			view, err := a.goalService().Get(a.session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if view.RoundsStarted != 1 || !strings.Contains(request.Messages[len(request.Messages)-1].Content[0].Text, "<goal_round>") {
				t.Fatal("round not sourced/admitted", view)
			}
			return goalUpdateCall(view, "complete", nil), nil
		case 4:
			last := request.Messages[len(request.Messages)-1]
			if !strings.Contains(last.Content[0].Text, "<goal_complete>") {
				t.Fatal("missing autonomous wrapup", last)
			}
			return core.Response{Content: []core.Block{{Type: "text", Text: "verified final answer"}}}, nil
		default:
			t.Fatal("completed goal continued")
			return core.Response{}, nil
		}
	}))
	if err := a.turn("Please carry out the full migration"); err != nil {
		t.Fatal(err)
	}
	view, err := a.goalService().Get(a.session.ID)
	if err != nil || view.Phase != "complete" || view.RoundsStarted != 1 || calls.Load() != 4 {
		t.Fatal(view, calls.Load(), err)
	}
}

func TestGoalBlockedThresholdRequiresExactCurrentRoundAndReason(t *testing.T) {
	var a *App
	var denied atomic.Int32
	a = goalFixture(t, goalModel(func(_ context.Context, request core.Request, _ func(core.Delta)) (core.Response, error) {
		last := request.Messages[len(request.Messages)-1]
		if last.Role == "user" && len(last.Content) > 0 && last.Content[0].Type == "tool_result" {
			if last.Content[0].IsError {
				if !strings.Contains(last.Content[0].Content.(string), "GOAL_TOOL_BLOCK_THRESHOLD") {
					t.Fatal(last.Content)
				}
				denied.Add(1)
				return core.Response{Content: []core.Block{{Type: "text", Text: "investigated the blocker again"}}}, nil
			}
		}
		if len(last.Content) > 0 && strings.Contains(last.Content[0].Text, "<goal_blocked>") {
			return core.Response{Content: []core.Block{{Type: "text", Text: "need user input"}}}, nil
		}
		view, err := a.goalService().Get(a.session.ID)
		if err != nil {
			t.Fatal(err)
		}
		return goalUpdateCall(view, "blocked", map[string]any{"blocked_reason": "the required upstream access is still unavailable"}), nil
	}))
	if err := a.goalCommand(`{"objective":"resolve blocker","max_goal_rounds":5}`); err != nil {
		t.Fatal(err)
	}
	view := waitGoal(t, a, "blocked")
	if view.RoundsStarted != 3 || view.BlockedReason.Code != "model-reported" || denied.Load() != 2 {
		t.Fatal(view, denied.Load())
	}
}

func TestGoalNonHumanInputCannotCreateAndOffDriverCallIsRejected(t *testing.T) {
	a := goalFixture(t, goalModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		return core.Response{Content: []core.Block{{Type: "tool_use", ID: "create", Name: "create_goal", Input: core.JSON(map[string]any{"objective": "not authorized"})}}}, nil
	}))
	if _, err := a.createGoal(a.ctx, core.ToolContext{SessionID: a.session.ID}, json.RawMessage(`{"objective":"fake driver"}`)); err == nil {
		t.Fatal("off-driver goal creation succeeded")
	}
	message := core.TextMessage("user", "scheduled task")
	message.Source = map[string]any{"kind": "schedule"}
	a.engine.UpdateOptions(func(options *agent.Options) { options.MaxSteps = 1 })
	_ = a.engine.Run(a.ctx, a.session.ID, message)
	view, err := a.goalService().Get(a.session.ID)
	if err != nil || view != nil {
		t.Fatal("nonhuman input created a goal", view, err)
	}
}
