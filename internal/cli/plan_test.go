package cli

import (
	"bufio"
	"context"
	"strings"
	"testing"

	"dsgo/internal/core"
)

func TestPlanModeCommitsAtNextStepAndCancelsOppositeSelection(t *testing.T) {
	a, s := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		return core.Response{}, nil
	}))
	_, _ = a.store.Append(s.ID, "turn1", "turn/started", nil)
	_, _ = a.store.Append(s.ID, "turn1", "plan/request", map[string]any{"active": false})
	if err := a.setPlan(s.ID, true); err != nil {
		t.Fatal(err)
	}
	if a.planFor(s.ID) || !a.planGuidanceFor(s.ID) {
		t.Fatal("open-turn selection committed before the next step")
	}
	if err := a.prepareStep(a.ctx, s, "turn1", 2); err != nil {
		t.Fatal(err)
	}
	if !a.planFor(s.ID) {
		t.Fatal("next step did not commit selection")
	}
	messages, err := a.store.Messages(s.ID)
	var narration []core.Message
	for _, message := range messages {
		if messageSourceKind(message) == "plan-mode" {
			narration = append(narration, message)
		}
	}
	if err != nil || len(narration) != 1 || narration[0].Content[0].Text != "The user switched this session to plan mode." {
		t.Fatalf("mode narration: %+v %v", messages, err)
	}
	if err := a.setPlan(s.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := a.setPlan(s.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := a.prepareStep(a.ctx, s, "turn1", 3); err != nil {
		t.Fatal(err)
	}
	if !a.planFor(s.ID) {
		t.Fatal("cancelled exit changed the mode")
	}
	events, _ := a.store.Events(s.ID)
	count := 0
	for _, event := range events {
		if event.Type == "plan/mode" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("mode transitions=%d", count)
	}
}

func TestPlanReviewUsesExplicitChoiceAndAppliesAfterToolBatch(t *testing.T) {
	for _, tc := range []struct {
		answer   string
		approved bool
	}{{"1\n", true}, {"Approve\n", true}, {"2\n", false}, {"yes\n", false}, {"Revise the rollback section\n", false}, {"3\nApprove\n", false}, {"Other\n1\n", false}, {"3\n2025\n", false}} {
		t.Run(strings.TrimSpace(tc.answer), func(t *testing.T) {
			a, s := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
				return core.Response{}, nil
			}))
			a.in = bufio.NewReader(strings.NewReader(tc.answer))
			if err := a.setPlan(s.ID, true); err != nil {
				t.Fatal(err)
			}
			_, _ = a.store.Append(s.ID, "turn1", "turn/started", nil)
			_, err := a.exitPlanMode(a.ctx, core.ToolContext{SessionID: s.ID}, core.JSON(map[string]any{"plan": "# Plan\nImplement and verify."}))
			if (err == nil) != tc.approved {
				t.Fatalf("approval=%v err=%v", tc.approved, err)
			}
			if !a.planFor(s.ID) {
				t.Fatal("review changed guidance in the same tool batch")
			}
			if err := a.prepareStep(a.ctx, s, "turn1", 2); err != nil {
				t.Fatal(err)
			}
			if a.planFor(s.ID) == tc.approved {
				t.Fatal("next-step mode does not match review")
			}
		})
	}
}

func TestPlanHeadingUsesOfficialWhitespace(t *testing.T) {
	for _, text := range []string{"# Plan", "\ufeff#\u00a0Plan\ufeff", "#\nPlan"} {
		if !validPlanHeading(text) {
			t.Fatalf("rejected %q", text)
		}
	}
	for _, text := range []string{"## Plan", "# ", "#\u0085Plan", "No heading"} {
		if validPlanHeading(text) {
			t.Fatalf("accepted %q", text)
		}
	}
}
