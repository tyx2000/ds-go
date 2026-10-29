package state

import (
	"encoding/json"
	"fmt"

	"dsgo/internal/core"
)

const MaxGoalInteger = 9007199254740991

type GoalError struct{ Code, Message string }

func (e *GoalError) Error() string         { return e.Code + ": " + e.Message }
func goalError(code, message string) error { return &GoalError{code, message} }

type GoalRef struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}
type GoalBlockReason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type GoalSnapshot struct {
	GoalRef
	Objective     string           `json:"objective"`
	Phase         string           `json:"phase"`
	MaxGoalRounds int64            `json:"maxGoalRounds"`
	BlockedReason *GoalBlockReason `json:"blockedReason,omitempty"`
}
type GoalView struct {
	GoalSnapshot
	RoundsStarted int64  `json:"roundsStarted"`
	CreatedAt     int64  `json:"createdAt"`
	UpdatedAt     int64  `json:"updatedAt"`
	Activation    string `json:"activation"`
}
type GoalEdit struct {
	Objective     *string
	MaxGoalRounds *int64
}
type GoalMutation struct {
	View  *GoalView
	Event core.Event
}
type GoalRoundSource struct {
	Kind     string `json:"kind"`
	GoalID   string `json:"goalId"`
	Revision int64  `json:"revision"`
	Round    int64  `json:"round"`
}
type GoalProjection struct {
	Goal                 *GoalSnapshot
	RoundsStarted        int64
	CreatedAt, UpdatedAt int64
	LegacyObjective      string
}
type goalChange struct {
	Kind          string       `json:"kind"`
	Version       int          `json:"version"`
	Operation     string       `json:"operation"`
	Goal          GoalSnapshot `json:"goal"`
	RoundsStarted int64        `json:"roundsStarted"`
	CreatedAt     int64        `json:"createdAt"`
	UpdatedAt     int64        `json:"updatedAt"`
}
type goalClear struct {
	Kind      string  `json:"kind"`
	Version   int     `json:"version"`
	Operation string  `json:"operation"`
	Cleared   GoalRef `json:"cleared"`
	ClearedAt int64   `json:"clearedAt"`
}

func GoalSource(message core.Message) (GoalRoundSource, bool, error) {
	var source GoalRoundSource
	encoded, err := json.Marshal(message.Source)
	if err != nil {
		return source, false, err
	}
	var kind struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(encoded, &kind); err != nil || kind.Kind != "goal" {
		return source, false, nil
	}
	if err := json.Unmarshal(encoded, &source); err != nil || source.GoalID == "" || source.Revision < 1 || source.Revision > MaxGoalInteger || source.Round < 1 || source.Round > MaxGoalInteger {
		return source, true, fmt.Errorf("invalid goal message source")
	}
	return source, true, nil
}

func cloneGoal(goal *GoalSnapshot) *GoalSnapshot {
	if goal == nil {
		return nil
	}
	copy := *goal
	if copy.BlockedReason != nil {
		reason := *copy.BlockedReason
		copy.BlockedReason = &reason
	}
	return &copy
}
