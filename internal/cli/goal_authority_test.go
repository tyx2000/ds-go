package cli

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"dsgo/internal/core"
)

func goalAuthorityModel(t *testing.T, allowed bool) core.Model {
	t.Helper()
	var calls atomic.Int32
	return goalModel(func(_ context.Context, req core.Request, _ func(core.Delta)) (core.Response, error) {
		if calls.Add(1) == 1 {
			return core.Response{Content: []core.Block{{Type: "tool_use", ID: "create", Name: "create_goal", Input: core.JSON(map[string]any{"objective": "root authority fixture", "max_goal_rounds": 1})}}}, nil
		}
		last := req.Messages[len(req.Messages)-1]
		if len(last.Content) != 1 || last.Content[0].Type != "tool_result" || last.Content[0].IsError == allowed {
			t.Fatalf("human authority allowed=%v, tool result %+v", allowed, last)
		}
		if !allowed {
			content, _ := last.Content[0].Content.(string)
			if !strings.Contains(content, "GOAL_TOOL_AUTHORITY_REQUIRED") {
				t.Fatalf("wrong denial: %q", content)
			}
		}
		return core.Response{Content: []core.Block{{Type: "text", Text: "authority checked"}}}, nil
	})
}

func runGoalHumanAuthority(t *testing.T, a *App, id string, allowed bool) {
	t.Helper()
	// This represents the foreground turn owner even if the user selects
	// another conversation while this admitted turn continues in background.
	a.foreground.Store(id, true)
	defer a.foreground.Delete(id)
	message := core.TextMessage("user", "Please complete the migration")
	message.Source = map[string]any{"kind": "user"}
	if err := a.engine.Run(a.ctx, id, message); err != nil {
		t.Fatal(err)
	}
	view, err := a.goalService().Get(id)
	if err != nil || (view != nil) != allowed {
		t.Fatalf("goal exists=%v, allowed=%v: %+v %v", view != nil, allowed, view, err)
	}
}

func TestGoalBackgroundRootKeepsHumanAuthorityAfterForegroundSwitch(t *testing.T) {
	a := goalFixture(t, goalAuthorityModel(t, true))
	root := a.session.ID
	other, err := a.store.Create(a.session.CWD, "other foreground conversation")
	if err != nil {
		t.Fatal(err)
	}
	a.current.Store(other.ID)
	runGoalHumanAuthority(t, a, root, true)
}

func TestGoalResidentDelegatedChildCannotGainHumanAuthorityBySelection(t *testing.T) {
	a := goalFixture(t, goalAuthorityModel(t, false))
	session, err := a.store.CreateChild(a.session.CWD, "resident child", a.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	session.Delegated = true
	if err := a.store.Update(session); err != nil {
		t.Fatal(err)
	}
	a.manager.mu.Lock()
	a.manager.children[session.ID] = &child{ID: session.ID, ParentID: a.session.ID, Status: "running", cancel: func() {}}
	a.manager.mu.Unlock()
	a.current.Store(session.ID)
	runGoalHumanAuthority(t, a, session.ID, false)
}

func TestGoalOwnedWorkflowMemberHasNoDirectHumanAuthority(t *testing.T) {
	a := goalFixture(t, goalAuthorityModel(t, false))
	session, err := a.store.CreateChild(a.session.CWD, "workflow member", a.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	release := a.ownGoalActivation(session.ID, a.session.ID)
	defer release()
	a.current.Store(session.ID)
	runGoalHumanAuthority(t, a, session.ID, false)
}

func TestGoalManualForkWithDelegatedLineageIsIndependentRuntimeRoot(t *testing.T) {
	a := goalFixture(t, goalAuthorityModel(t, true))
	childSession, err := a.store.CreateChild(a.session.CWD, "historical delegated child", a.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	childSession.Delegated = true
	if err := a.store.Update(childSession); err != nil {
		t.Fatal(err)
	}
	fork, err := a.store.Fork(childSession.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !fork.Delegated || fork.ParentID != childSession.ID {
		t.Fatal("fixture lost its durable delegated ancestry")
	}
	// No owning live activation establishes this fork. The historical flags
	// remain valid policy metadata, while goal authority follows runtime roots.
	runGoalHumanAuthority(t, a, fork.ID, true)
}
