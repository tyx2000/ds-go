package state

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"dsgo/internal/core"
)

func goalErrorCode(err error) string {
	var failure *GoalError
	if errors.As(err, &failure) {
		return failure.Code
	}
	return ""
}
func TestGoalsCASLifecycleAndClearTombstone(t *testing.T) {
	store, session := testStore(t)
	goals := NewGoals(store)
	created, err := goals.Create(session.ID, "", "  objective  ", 4)
	if err != nil || created.View.Objective != "objective" || created.View.Revision != 1 || created.View.Activation != "armed" {
		t.Fatal(created, err)
	}
	if _, err := goals.Create(session.ID, "", "another", 4); goalErrorCode(err) != "GOAL_ALREADY_EXISTS" {
		t.Fatal(err)
	}
	paused, err := goals.Update(session.ID, "", created.View.GoalRef, "pause", GoalEdit{}, nil)
	if err != nil || paused.View.Phase != "paused" || paused.View.Activation != "disarmed" {
		t.Fatal(paused, err)
	}
	if _, err := goals.Update(session.ID, "", created.View.GoalRef, "resume", GoalEdit{}, nil); goalErrorCode(err) != "GOAL_STALE_REVISION" {
		t.Fatal(err)
	}
	resumed, err := goals.Update(session.ID, "", paused.View.GoalRef, "resume", GoalEdit{}, nil)
	if err != nil || resumed.View.Revision != 3 || resumed.View.Activation != "armed" {
		t.Fatal(resumed, err)
	}
	cleared, err := goals.Update(session.ID, "", resumed.View.GoalRef, "clear", GoalEdit{}, nil)
	if err != nil || cleared.View != nil {
		t.Fatal(cleared, err)
	}
	current, err := goals.Get(session.ID)
	if err != nil || current != nil {
		t.Fatal(current, err)
	}
	var tombstone goalClear
	if err := json.Unmarshal(cleared.Event.Data, &tombstone); err != nil || tombstone.Cleared.Revision != 4 {
		t.Fatal(tombstone, err)
	}
}

func TestGoalsReservationAdmissionBudgetAndForkDisarm(t *testing.T) {
	store, session := testStore(t)
	goals := NewGoals(store)
	created, err := goals.Create(session.ID, "", "complete objective", 1)
	if err != nil {
		t.Fatal(err)
	}
	message, _, err := goals.ReserveRound(session.ID)
	if err != nil || message == nil {
		t.Fatal(message, err)
	}
	before, _ := goals.Get(session.ID)
	if before.RoundsStarted != 0 {
		t.Fatal("reservation spent round allowance")
	}
	if duplicate, _, err := goals.ReserveRound(session.ID); err != nil || duplicate != nil {
		t.Fatal("duplicate reservation", duplicate, err)
	}
	if _, err := goals.AdmitRound(context.Background(), session.ID, "round-turn", *message, nil); err != nil {
		t.Fatal(err)
	}
	if err := goals.BeforeModel(session.ID, "round-turn"); err != nil {
		t.Fatal(err)
	}
	if _, err := goals.FinishTurn(session.ID, "round-turn", "completed", false); err != nil {
		t.Fatal(err)
	}
	if message, event, err := goals.ReserveRound(session.ID); err != nil || message != nil || event == nil {
		t.Fatal("round cap did not block", message, event, err)
	}
	blocked, err := goals.Get(session.ID)
	if err != nil || blocked.RoundsStarted != 1 || blocked.Phase != "blocked" || blocked.BlockedReason.Code != "round-limit" {
		t.Fatal(blocked, err)
	}
	if _, err := goals.Update(session.ID, "", blocked.GoalRef, "resume", GoalEdit{}, nil); goalErrorCode(err) != "GOAL_INVALID_TRANSITION" {
		t.Fatal(err)
	}
	cap := int64(2)
	edited, err := goals.Update(session.ID, "", blocked.GoalRef, "edit", GoalEdit{MaxGoalRounds: &cap}, nil)
	if err != nil || edited.View.Phase != "blocked" {
		t.Fatal(edited, err)
	}
	resumed, err := goals.Update(session.ID, "", edited.View.GoalRef, "resume", GoalEdit{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewGoals(store)
	read, err := restarted.Get(session.ID)
	if err != nil || read.Activation != "disarmed" || read.RoundsStarted != 1 || read.ID != created.View.ID {
		t.Fatal(read, err)
	}
	child, err := store.Fork(session.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	inherited, err := goals.Get(child.ID)
	if err != nil || inherited.Activation != "disarmed" || inherited.RoundsStarted != 1 {
		t.Fatal(inherited, err)
	}
	if resumed.View.Activation != "armed" {
		t.Fatal("resume did not arm")
	}
}

func TestGoalsConcurrentCASAndStaleRoundFence(t *testing.T) {
	store, session := testStore(t)
	first, second := NewGoals(store), NewGoals(store)
	created, err := first.Create(session.ID, "", "original", 5)
	if err != nil {
		t.Fatal(err)
	}
	message, _, err := first.ReserveRound(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	wait.Add(2)
	results := make(chan error, 2)
	for i, service := range []*Goals{first, second} {
		go func(index int, g *Goals) {
			defer wait.Done()
			objective := []string{"one", "two"}[index]
			_, err := g.Update(session.ID, "", created.View.GoalRef, "edit", GoalEdit{Objective: &objective}, nil)
			results <- err
		}(i, service)
	}
	wait.Wait()
	close(results)
	success, stale := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if goalErrorCode(err) == "GOAL_STALE_REVISION" {
			stale++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatal("CAS was not exclusive", success, stale)
	}
	if _, err := first.AdmitRound(context.Background(), session.ID, "stale", *message, nil); goalErrorCode(err) != "GOAL_ROUND_STALE" {
		t.Fatal(err)
	}
	view, err := first.Get(session.ID)
	if err != nil || view.RoundsStarted != 0 {
		t.Fatal("stale reservation consumed round", view, err)
	}
}

func TestGoalsCancelPausesOnlyMatchingAttemptAndLegacyIsReadOnly(t *testing.T) {
	store, session := testStore(t)
	_, _ = store.Append(session.ID, "", "goal/updated", map[string]any{"objective": "legacy objective", "status": "active", "token_budget": 1})
	goals := NewGoals(store)
	if view, err := goals.Get(session.ID); err != nil || view != nil {
		t.Fatal("legacy status granted native authority", view, err)
	}
	if objective, err := goals.LegacyObjective(session.ID); err != nil || objective != "legacy objective" {
		t.Fatal(objective, err)
	}
	created, err := goals.Create(session.ID, "", "native objective", 4)
	if err != nil {
		t.Fatal(err)
	}
	if objective, err := goals.LegacyObjective(session.ID); err != nil || objective != "" {
		t.Fatal("native facts did not supersede legacy", objective, err)
	}
	message, _, err := goals.ReserveRound(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := goals.AdmitRound(context.Background(), session.ID, "cancelled-round", *message, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := goals.FinishTurn(session.ID, "cancelled-round", "cancelled", false); err != nil {
		t.Fatal(err)
	}
	paused, err := goals.Get(session.ID)
	if err != nil || paused.Phase != "paused" || paused.Activation != "disarmed" || paused.RoundsStarted != 1 {
		t.Fatal(paused, err)
	}
	resumed, err := goals.Update(session.ID, "", paused.GoalRef, "resume", GoalEdit{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := goals.FinishTurn(session.ID, "ordinary-turn", "cancelled", false); err != nil {
		t.Fatal(err)
	}
	disarmed, err := goals.Get(session.ID)
	if err != nil || disarmed.Phase != "active" || disarmed.Activation != "disarmed" || disarmed.Revision != resumed.View.Revision || disarmed.ID != created.View.ID {
		t.Fatal(disarmed, err)
	}
}

func TestGoalsStrictReplayRejectsTamperedFactsAndNonsequentialRounds(t *testing.T) {
	for _, failure := range []string{"unknown-field", "revision-gap", "round-gap"} {
		t.Run(failure, func(t *testing.T) {
			store, session := testStore(t)
			goals := NewGoals(store)
			created, err := goals.Create(session.ID, "", "objective", 4)
			if err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "unknown-field":
				var data map[string]any
				_ = json.Unmarshal(created.Event.Data, &data)
				data["unknown"] = true
				_, _ = store.Append(session.ID, "", "goal/change", data)
			case "revision-gap":
				var data goalChange
				_ = json.Unmarshal(created.Event.Data, &data)
				data.Operation = "pause"
				data.Goal.Phase = "paused"
				data.Goal.Revision = 3
				_, _ = store.Append(session.ID, "", "goal/change", data)
			case "round-gap":
				message := core.TextMessage("user", "forged round")
				message.Source = GoalRoundSource{Kind: "goal", GoalID: created.View.ID, Revision: 1, Round: 2}
				_, _ = store.Append(session.ID, "", "user/message", message)
			}
			if _, err := NewGoals(store).Get(session.ID); err == nil {
				t.Fatal("malformed native goal replay was accepted")
			}
		})
	}
}
