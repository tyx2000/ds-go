package state

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"dsgo/internal/core"
)

type goalAttempt struct {
	message           core.Message
	source            GoalRoundSource
	turn              string
	admitted, started bool
}
type goalActivation struct {
	armed   bool
	ref     GoalRef
	attempt *goalAttempt
}

// Goals separates durable objective/round facts from process-local authority.
// A new service and every Open edge start disarmed.
type Goals struct {
	store *Store
	mu    sync.Mutex
	live  map[string]*goalActivation
}

func NewGoals(store *Store) *Goals {
	return &Goals{store: store, live: make(map[string]*goalActivation)}
}
func (g *Goals) activation(id string) *goalActivation {
	runtime := g.live[id]
	if runtime == nil {
		runtime = &goalActivation{}
		g.live[id] = runtime
	}
	return runtime
}
func (g *Goals) Open(id string) { g.mu.Lock(); defer g.mu.Unlock(); g.live[id] = &goalActivation{} }
func (g *Goals) Disarm(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	runtime := g.activation(id)
	runtime.armed = false
}
func (g *Goals) DisarmAll() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, runtime := range g.live {
		runtime.armed = false
	}
}
func (g *Goals) read(id string) (GoalProjection, error) {
	events, err := g.store.Events(id)
	if err != nil {
		return GoalProjection{}, err
	}
	return FoldGoal(events)
}
func (g *Goals) view(id string, projection GoalProjection) *GoalView {
	runtime := g.activation(id)
	if projection.Goal == nil {
		runtime.armed = false
		return nil
	}
	if runtime.ref != projection.Goal.GoalRef {
		runtime.armed = false
	}
	activation := "disarmed"
	if runtime.armed {
		activation = "armed"
	}
	return &GoalView{GoalSnapshot: *cloneGoal(projection.Goal), RoundsStarted: projection.RoundsStarted, CreatedAt: projection.CreatedAt, UpdatedAt: projection.UpdatedAt, Activation: activation}
}
func (g *Goals) Get(id string) (*GoalView, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	projection, err := g.read(id)
	if err != nil {
		return nil, err
	}
	return g.view(id, projection), nil
}
func (g *Goals) LegacyObjective(id string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	projection, err := g.read(id)
	return projection.LegacyObjective, err
}

func (g *Goals) Create(id, turn, objective string, maxRounds int64) (GoalMutation, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if strings.TrimSpace(objective) == "" {
		return GoalMutation{}, goalError("GOAL_INVALID_OBJECTIVE", "goal objective must be non-empty")
	}
	if maxRounds < 1 || maxRounds > MaxGoalInteger {
		return GoalMutation{}, goalError("GOAL_INVALID_MAX_ROUNDS", "maxGoalRounds must be a positive safe integer")
	}
	var result GoalMutation
	err := g.store.withLock(func() error {
		events, err := g.store.events(id)
		if err != nil {
			return err
		}
		projection, err := FoldGoal(events)
		if err != nil {
			return err
		}
		if projection.Goal != nil && projection.Goal.Phase != "complete" {
			return goalError("GOAL_ALREADY_EXISTS", "an unfinished goal already exists")
		}
		random, err := newID()
		if err != nil {
			return err
		}
		now := max(int64(0), time.Now().UnixMilli())
		snapshot := GoalSnapshot{GoalRef: GoalRef{ID: "goal-" + random, Revision: 1}, Objective: strings.TrimSpace(objective), Phase: "active", MaxGoalRounds: maxRounds}
		change := goalChange{Kind: "goal/change", Version: 1, Operation: "create", Goal: snapshot, CreatedAt: now, UpdatedAt: now}
		event, err := g.store.append(id, turn, "goal/change", change)
		if err != nil {
			g.activation(id).armed = false
			return err
		}
		runtime := g.activation(id)
		runtime.armed = true
		runtime.ref = snapshot.GoalRef
		projection = GoalProjection{Goal: &snapshot, CreatedAt: now, UpdatedAt: now}
		result = GoalMutation{View: g.view(id, projection), Event: event}
		return nil
	})
	return result, err
}

func (g *Goals) Update(id, turn string, ref GoalRef, operation string, edit GoalEdit, reason *GoalBlockReason) (GoalMutation, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.updateLocked(id, turn, ref, operation, edit, reason)
}
func (g *Goals) updateLocked(id, turn string, ref GoalRef, operation string, edit GoalEdit, reason *GoalBlockReason) (GoalMutation, error) {
	var result GoalMutation
	err := g.store.withLock(func() error {
		events, err := g.store.events(id)
		if err != nil {
			return err
		}
		projection, err := FoldGoal(events)
		if err != nil {
			return err
		}
		current := projection.Goal
		if current == nil {
			return goalError("GOAL_NOT_FOUND", "no current goal")
		}
		g.view(id, projection) // An external mutation cannot inherit live authority.
		if ref != current.GoalRef {
			return goalError("GOAL_STALE_REVISION", "goal id/revision is stale; call get_goal again")
		}
		if current.Revision >= MaxGoalInteger {
			return goalError("GOAL_INVALID_TRANSITION", "goal revision is exhausted")
		}
		next := *cloneGoal(current)
		next.Revision++
		runtime := g.activation(id)
		armed := runtime.armed
		switch operation {
		case "edit":
			if edit.Objective == nil && edit.MaxGoalRounds == nil {
				return goalError("GOAL_INVALID_EDIT", "edit requires objective and/or maxGoalRounds")
			}
			if edit.Objective != nil {
				next.Objective = strings.TrimSpace(*edit.Objective)
			}
			if edit.MaxGoalRounds != nil {
				next.MaxGoalRounds = *edit.MaxGoalRounds
			}
		case "pause":
			if current.Phase != "active" {
				return goalError("GOAL_INVALID_TRANSITION", "only an active goal may be paused")
			}
			next.Phase = "paused"
			next.BlockedReason = nil
			armed = false
		case "resume":
			if current.Phase == "complete" || current.Phase == "active" && armed || projection.RoundsStarted >= current.MaxGoalRounds {
				return goalError("GOAL_INVALID_TRANSITION", "goal is already armed, complete, or has exhausted its round allowance")
			}
			next.Phase = "active"
			next.BlockedReason = nil
			armed = true
		case "complete":
			if current.Phase == "complete" {
				return goalError("GOAL_INVALID_TRANSITION", "goal is already complete")
			}
			next.Phase = "complete"
			next.BlockedReason = nil
			armed = false
		case "block":
			if current.Phase != "active" || reason == nil {
				return goalError("GOAL_INVALID_TRANSITION", "only an active goal with a blocker reason may be blocked")
			}
			next.Phase = "blocked"
			copy := *reason
			copy.Message = strings.TrimSpace(copy.Message)
			next.BlockedReason = &copy
			armed = false
		case "clear":
			event, err := g.store.append(id, turn, "goal/change", goalClear{Kind: "goal/change", Version: 1, Operation: "clear", Cleared: next.GoalRef, ClearedAt: max(time.Now().UnixMilli(), projection.UpdatedAt)})
			if err != nil {
				runtime.armed = false
				return err
			}
			runtime.armed = false
			runtime.ref = GoalRef{}
			result = GoalMutation{Event: event}
			return nil
		default:
			return goalError("GOAL_INVALID_TRANSITION", "unknown goal operation")
		}
		if err := validGoalSnapshot(next); err != nil {
			return goalError("GOAL_INVALID_EDIT", err.Error())
		}
		if next.MaxGoalRounds < projection.RoundsStarted {
			return goalError("GOAL_INVALID_MAX_ROUNDS", "round cap cannot be below already admitted rounds")
		}
		change := goalChange{Kind: "goal/change", Version: 1, Operation: operation, Goal: next, RoundsStarted: projection.RoundsStarted, CreatedAt: projection.CreatedAt, UpdatedAt: max(time.Now().UnixMilli(), projection.UpdatedAt)}
		event, err := g.store.append(id, turn, "goal/change", change)
		if err != nil {
			runtime.armed = false
			return err
		}
		runtime.armed = armed
		runtime.ref = next.GoalRef
		projection.Goal = &next
		projection.UpdatedAt = change.UpdatedAt
		result = GoalMutation{View: g.view(id, projection), Event: event}
		return nil
	})
	return result, err
}

func RenderGoalRound(goal GoalView, round int64) string {
	return "<goal_round>\nObjective: " + string(core.JSON(goal.Objective)) + fmt.Sprintf("\nRound: %d/%d\n\n", round, goal.MaxGoalRounds) +
		"Continue working toward the objective in this same session. Treat the current workspace, tool results, and durable session state as authoritative; inspect them instead of assuming earlier narration is still current. Make concrete progress and verify the result. Before claiming completion, gather evidence that the whole objective is achieved, read the current goal, and mark it complete. If work remains, leave the goal active for the next round. Follow the configured goal-tool policy before reporting a blocker.\n</goal_round>"
}
func (g *Goals) ReserveRound(id string) (*core.Message, *core.Event, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	projection, err := g.read(id)
	if err != nil {
		return nil, nil, err
	}
	view := g.view(id, projection)
	runtime := g.activation(id)
	if view == nil || view.Phase != "active" || view.Activation != "armed" || runtime.attempt != nil {
		return nil, nil, nil
	}
	if view.RoundsStarted >= view.MaxGoalRounds {
		mutation, err := g.updateLocked(id, "", view.GoalRef, "block", GoalEdit{}, &GoalBlockReason{Code: "round-limit", Message: fmt.Sprintf("Goal reached its configured limit of %d rounds.", view.MaxGoalRounds)})
		if err != nil {
			return nil, nil, err
		}
		return nil, &mutation.Event, nil
	}
	random, err := newID()
	if err != nil {
		return nil, nil, err
	}
	source := GoalRoundSource{Kind: "goal", GoalID: view.ID, Revision: view.Revision, Round: view.RoundsStarted + 1}
	message := core.TextMessage("user", RenderGoalRound(*view, source.Round))
	message.Source = source
	message.DeliveryID = "goal:" + random
	runtime.attempt = &goalAttempt{message: message, source: source}
	return &message, nil, nil
}
func (g *Goals) DropRound(id, delivery string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	runtime := g.activation(id)
	if runtime.attempt != nil && runtime.attempt.message.DeliveryID == delivery && !runtime.attempt.admitted {
		runtime.attempt = nil
	}
}

// AdmitRound is the final revision/authority fence and durable round counter.
// The same local lock serializes disarm/pause with the atomic store append.
func (g *Goals) AdmitRound(ctx context.Context, id, turn string, message core.Message, contexts []core.Message) (core.Event, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var event core.Event
	runtime := g.activation(id)
	attempt := runtime.attempt
	source, isRound, sourceErr := GoalSource(message)
	if sourceErr != nil {
		return event, sourceErr
	}
	if attempt == nil || attempt.admitted || !isRound || source != attempt.source || message.DeliveryID != attempt.message.DeliveryID || message.Role != "user" || !bytes.Equal(core.JSON(message.Content), core.JSON(attempt.message.Content)) {
		return event, goalError("GOAL_ROUND_STALE", "goal round has no matching live reservation")
	}
	err := g.store.withLock(func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		events, err := g.store.events(id)
		if err != nil {
			return err
		}
		projection, err := FoldGoal(events)
		if err != nil {
			return err
		}
		view := g.view(id, projection)
		if view == nil || view.Phase != "active" || view.Activation != "armed" || view.GoalRef != (GoalRef{ID: attempt.source.GoalID, Revision: attempt.source.Revision}) || view.RoundsStarted+1 != attempt.source.Round {
			return goalError("GOAL_ROUND_STALE", "goal changed before round admission")
		}
		if pendingGoalCompetition(events) {
			return goalError("GOAL_ROUND_YIELDED", "accepted human or producer input takes priority")
		}
		if len(contexts) == 0 {
			event, err = g.store.append(id, turn, "user/message", message)
		} else {
			event, err = g.store.append(id, turn, "user/input", core.InputBatch{Version: 1, Message: message, Contexts: contexts})
		}
		if err != nil {
			runtime.armed = false
			return err
		}
		attempt.admitted = true
		attempt.turn = turn
		return nil
	})
	if err != nil && !attempt.admitted {
		runtime.attempt = nil
	}
	return event, err
}

func pendingGoalCompetition(events []core.Event) bool {
	delivered := map[string]bool{}
	for _, event := range events {
		messages, ok, err := core.UserInputMessages(event)
		if err == nil && ok && messages[0].DeliveryID != "" {
			delivered[messages[0].DeliveryID] = true
		}
	}
	for _, event := range events {
		if event.Type != "user/inbox" {
			continue
		}
		var entry struct {
			ID      string
			Message core.Message
		}
		if json.Unmarshal(event.Data, &entry) == nil && !delivered["inbox:"+entry.ID] {
			return true
		}
	}
	return false
}

func (g *Goals) BeforeModel(id, turn string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	runtime := g.activation(id)
	attempt := runtime.attempt
	if attempt == nil || !attempt.admitted || attempt.turn != turn || attempt.started {
		return nil
	}
	projection, err := g.read(id)
	if err != nil {
		runtime.armed = false
		return err
	}
	view := g.view(id, projection)
	if view == nil || view.Phase != "active" || view.Activation != "armed" || view.ID != attempt.source.GoalID || view.Revision != attempt.source.Revision || view.RoundsStarted != attempt.source.Round {
		return goalError("GOAL_ROUND_STALE", "goal changed before its first model request")
	}
	attempt.started = true
	return nil
}
func (g *Goals) FinishTurn(id, turn, status string, stale bool) (*core.Event, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	runtime := g.activation(id)
	attempt := runtime.attempt
	if attempt != nil && (attempt.turn == turn || !attempt.admitted) {
		runtime.attempt = nil
		projection, err := g.read(id)
		if err != nil {
			runtime.armed = false
			return nil, err
		}
		view := g.view(id, projection)
		same := view != nil && view.ID == attempt.source.GoalID && view.Revision == attempt.source.Revision
		if status == "cancelled" && same && view.Phase == "active" && view.Activation == "armed" {
			mutation, err := g.updateLocked(id, "", view.GoalRef, "pause", GoalEdit{}, nil)
			if err != nil {
				runtime.armed = false
				return nil, err
			}
			return &mutation.Event, nil
		}
		if status != "completed" && !stale && same {
			runtime.armed = false
		}
		return nil, nil
	}
	if status != "completed" && !stale {
		runtime.armed = false
	}
	return nil, nil
}
