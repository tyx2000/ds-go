package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"dsgo/internal/core"
)

var goalBlockCode = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)

func goalKeys(raw json.RawMessage, names ...string) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return errors.New("expected goal record")
	}
	got := make([]string, 0, len(object))
	for name := range object {
		got = append(got, name)
	}
	sort.Strings(got)
	sort.Strings(names)
	if strings.Join(got, ",") != strings.Join(names, ",") {
		return fmt.Errorf("unexpected goal record fields: %s", strings.Join(got, ","))
	}
	return nil
}
func validGoalRef(ref GoalRef) bool {
	return ref.ID != "" && ref.Revision > 0 && ref.Revision <= MaxGoalInteger
}
func validGoalSnapshot(goal GoalSnapshot) error {
	if !validGoalRef(goal.GoalRef) || strings.TrimSpace(goal.Objective) == "" || goal.Objective != strings.TrimSpace(goal.Objective) || goal.MaxGoalRounds < 1 || goal.MaxGoalRounds > MaxGoalInteger {
		return errors.New("invalid goal snapshot identity, objective or round cap")
	}
	switch goal.Phase {
	case "active", "paused", "complete":
		if goal.BlockedReason != nil {
			return errors.New("only a blocked goal has a blocker reason")
		}
	case "blocked":
		if goal.BlockedReason == nil || !goalBlockCode.MatchString(goal.BlockedReason.Code) || strings.TrimSpace(goal.BlockedReason.Message) == "" || goal.BlockedReason.Message != strings.TrimSpace(goal.BlockedReason.Message) {
			return errors.New("invalid goal blocker reason")
		}
	default:
		return errors.New("invalid goal phase")
	}
	return nil
}

// FoldGoal is strict over native goal facts and admitted round messages.
// Historical dsgo goal/updated facts contribute only an objective diagnostic;
// their token budgets and active flags never authorize automatic continuation.
func FoldGoal(events []core.Event) (GoalProjection, error) {
	var state GoalProjection
	seen := map[string]bool{}
	nativeSeen := false
	for _, event := range events {
		if event.Type == "goal/updated" {
			var old struct{ Objective string }
			if !nativeSeen && json.Unmarshal(event.Data, &old) == nil && strings.TrimSpace(old.Objective) != "" {
				state.LegacyObjective = strings.TrimSpace(old.Objective)
			}
			continue
		}
		if event.Type == "goal/change" {
			if err := applyGoalChange(&state, seen, event.Data); err != nil {
				return GoalProjection{}, fmt.Errorf("goal replay failed at session event %d: %w", event.Seq, err)
			}
			nativeSeen = true
			state.LegacyObjective = ""
			continue
		}
		messages, recognized, err := core.UserInputMessages(event)
		if err != nil {
			return GoalProjection{}, fmt.Errorf("goal input replay failed at session event %d: %w", event.Seq, err)
		}
		if !recognized {
			continue
		}
		for _, message := range messages {
			source, round, err := GoalSource(message)
			if err != nil {
				return GoalProjection{}, fmt.Errorf("goal round replay failed at event %d: %w", event.Seq, err)
			}
			if !round {
				continue
			}
			current := state.Goal
			if current == nil || current.Phase != "active" || source.GoalID != current.ID || source.Revision != current.Revision || source.Round != state.RoundsStarted+1 || source.Round > current.MaxGoalRounds {
				return GoalProjection{}, fmt.Errorf("goal round at event %d is not the next round of the active goal", event.Seq)
			}
			state.RoundsStarted = source.Round
		}
	}
	state.Goal = cloneGoal(state.Goal)
	return state, nil
}

func applyGoalChange(state *GoalProjection, seen map[string]bool, raw json.RawMessage) error {
	var header struct {
		Kind      string
		Version   int
		Operation string
	}
	if err := json.Unmarshal(raw, &header); err != nil || header.Kind != "goal/change" || header.Version != 1 {
		return errors.New("invalid goal/change kind or version")
	}
	current := state.Goal
	if header.Operation == "clear" {
		if err := goalKeys(raw, "kind", "version", "operation", "cleared", "clearedAt"); err != nil {
			return err
		}
		var change goalClear
		if err := json.Unmarshal(raw, &change); err != nil {
			return err
		}
		if current == nil || !validGoalRef(change.Cleared) || change.Cleared.ID != current.ID || change.Cleared.Revision != current.Revision+1 || change.ClearedAt < state.UpdatedAt {
			return errors.New("invalid goal clear transition")
		}
		state.Goal, state.RoundsStarted, state.CreatedAt, state.UpdatedAt = nil, 0, 0, 0
		return nil
	}
	if err := goalKeys(raw, "kind", "version", "operation", "goal", "roundsStarted", "createdAt", "updatedAt"); err != nil {
		return err
	}
	var shape struct{ Goal json.RawMessage }
	if err := json.Unmarshal(raw, &shape); err != nil {
		return err
	}
	var change goalChange
	if err := json.Unmarshal(raw, &change); err != nil {
		return err
	}
	next := change.Goal
	names := []string{"id", "revision", "objective", "phase", "maxGoalRounds"}
	if next.Phase == "blocked" {
		names = append(names, "blockedReason")
	}
	if err := goalKeys(shape.Goal, names...); err != nil {
		return err
	}
	if err := validGoalSnapshot(next); err != nil {
		return err
	}
	if next.BlockedReason != nil {
		var object map[string]json.RawMessage
		_ = json.Unmarshal(shape.Goal, &object)
		if err := goalKeys(object["blockedReason"], "code", "message"); err != nil {
			return err
		}
	}
	if change.RoundsStarted < 0 || change.RoundsStarted > next.MaxGoalRounds || change.CreatedAt < 0 || change.UpdatedAt < change.CreatedAt {
		return errors.New("invalid goal counters or timestamps")
	}
	if change.Operation == "create" {
		if next.Revision != 1 || next.Phase != "active" || change.RoundsStarted != 0 || current != nil && current.Phase != "complete" || seen[next.ID] {
			return errors.New("goal create requires a fresh active revision-one identity with zero rounds")
		}
		seen[next.ID] = true
	} else {
		if current == nil || next.ID != current.ID || next.Revision != current.Revision+1 || change.CreatedAt != state.CreatedAt || change.UpdatedAt < state.UpdatedAt || change.RoundsStarted != state.RoundsStarted {
			return errors.New("goal change must preserve identity, counters, timestamps and advance one revision")
		}
		if change.Operation != "edit" && (next.Objective != current.Objective || next.MaxGoalRounds != current.MaxGoalRounds) {
			return errors.New("only goal edit changes the definition")
		}
		switch change.Operation {
		case "edit":
			if next.Phase != current.Phase || string(core.JSON(next.BlockedReason)) != string(core.JSON(current.BlockedReason)) {
				return errors.New("goal edit cannot change phase or blocker")
			}
		case "pause":
			if current.Phase != "active" || next.Phase != "paused" {
				return errors.New("invalid goal pause transition")
			}
		case "resume":
			if current.Phase == "complete" || next.Phase != "active" || state.RoundsStarted >= next.MaxGoalRounds {
				return errors.New("invalid goal resume transition or exhausted round allowance")
			}
		case "complete":
			if current.Phase == "complete" || next.Phase != "complete" {
				return errors.New("invalid goal complete transition")
			}
		case "block":
			if current.Phase != "active" || next.Phase != "blocked" {
				return errors.New("invalid goal block transition")
			}
		default:
			return errors.New("unknown goal change operation")
		}
	}
	state.Goal, state.RoundsStarted, state.CreatedAt, state.UpdatedAt = cloneGoal(&next), change.RoundsStarted, change.CreatedAt, change.UpdatedAt
	return nil
}
