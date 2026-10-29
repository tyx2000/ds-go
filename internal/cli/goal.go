package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"dsgo/internal/agent"
	"dsgo/internal/core"
	"dsgo/internal/state"
)

type goalRuntime struct {
	mu      sync.Mutex
	service *state.Goals
	drivers map[string]*goalDriver
	owned   map[string]*goalOwnedActivation
	closing bool
	wg      sync.WaitGroup
}
type goalDriver struct {
	cancel    context.CancelFunc
	requested bool
	done      chan struct{}
}

type goalOwnedActivation struct{ parent string }

// ownGoalActivation records runtime ownership for fixed producers which drive
// Engine sessions outside the subagent manager, such as workflow members.
// Durable ParentID/Delegated metadata is not this relation: an independent
// manual fork or cold top-level resume may retain that metadata and be a root.
func (a *App) ownGoalActivation(id, parent string) func() {
	entry := &goalOwnedActivation{parent: parent}
	a.goals.mu.Lock()
	if a.goals.owned == nil {
		a.goals.owned = make(map[string]*goalOwnedActivation)
	}
	a.goals.owned[id] = entry
	a.goals.mu.Unlock()
	return func() {
		a.goals.mu.Lock()
		if a.goals.owned[id] == entry {
			delete(a.goals.owned, id)
		}
		a.goals.mu.Unlock()
	}
}

func (a *App) goalRuntimeRoot(id string) bool {
	a.goals.mu.Lock()
	owned := a.goals.owned[id] != nil
	a.goals.mu.Unlock()
	if owned {
		return false
	}
	if a.manager == nil {
		return true
	}
	a.manager.mu.Lock()
	defer a.manager.mu.Unlock()
	return !a.manager.closing && a.manager.children[id] == nil
}

func (a *App) goalService() *state.Goals {
	a.goals.mu.Lock()
	defer a.goals.mu.Unlock()
	if a.goals.service == nil && a.store != nil {
		a.goals.service = state.NewGoals(a.store)
	}
	return a.goals.service
}
func goalFailure(code, message string) error { return &state.GoalError{Code: code, Message: message} }
func (a *App) goalDefaults() (int64, int64) {
	maxRounds, blocked := int64(a.cfg.GoalDefaultMaxRounds), int64(a.cfg.GoalBlockedAfterRounds)
	if maxRounds == 0 {
		maxRounds = 256
	}
	if blocked == 0 {
		blocked = 3
	}
	return maxRounds, blocked
}
func goalValue(view *state.GoalView) core.ToolResult {
	if view == nil {
		return toolValue(map[string]any{"goal": nil})
	}
	value := map[string]any{"id": view.ID, "revision": view.Revision, "objective": view.Objective, "phase": view.Phase, "roundsStarted": view.RoundsStarted, "maxGoalRounds": view.MaxGoalRounds}
	if view.BlockedReason != nil {
		value["blockedReason"] = view.BlockedReason
	}
	return toolValue(map[string]any{"goal": value, "activation": view.Activation})
}
func (a *App) goalGuidance() string {
	_, blocked := a.goalDefaults()
	return fmt.Sprintf("create_goal may infer goal intent from a direct human request in any language. After session resume or fork, an active goal is disarmed: when a human asks to continue or resume in any wording or language, use update_goal action resume to rearm it. Mark complete only when the objective is actually achieved. Mark blocked only after the same blocking condition persists for at least %d consecutive goal rounds, and report that concrete condition in blocked_reason; difficulty, uncertainty, or useful remaining work is not blocked.", blocked)
}
func (a *App) commitGoalInput(ctx context.Context, session core.Session, turn string, message core.Message, contexts []core.Message) (core.Event, bool, error) {
	_, round, err := state.GoalSource(message)
	if err != nil {
		return core.Event{}, round, err
	}
	if !round {
		return core.Event{}, false, nil
	}
	event, err := a.goalService().AdmitRound(ctx, session.ID, turn, message, contexts)
	return event, true, err
}
func (a *App) beforeGoalModel(ctx context.Context, session core.Session, turn string, _ core.Request) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if service := a.goalService(); service != nil {
		return service.BeforeModel(session.ID, turn)
	}
	return nil
}
func (a *App) goalOpened(id string) {
	if id == "" || a.store == nil {
		return
	}
	a.goalService().Open(id)
}
func (a *App) dropGoalReservation(id, delivery string) {
	if service := a.goalService(); service != nil {
		service.DropRound(id, delivery)
	}
}
func (a *App) goalContinuation(ctx context.Context, id string) (*core.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a.engine == nil || a.engine.Busy(id) {
		return nil, nil
	}
	if pending, err := a.engine.PendingInbox(id); err != nil || pending {
		return nil, err
	}
	if a.manager != nil {
		if err := a.manager.waitProgressingChildren(ctx, id); err != nil {
			return nil, err
		}
		if a.engine.Busy(id) {
			return nil, nil
		}
	}
	message, event, err := a.goalService().ReserveRound(id)
	if event != nil {
		a.notice(agent.Notice{SessionID: id, Type: "event", Data: *event})
	}
	return message, err
}
func (a *App) goalNotice(notice agent.Notice) {
	if a.store == nil || notice.Type != "event" {
		return
	}
	event, ok := notice.Data.(core.Event)
	if !ok {
		return
	}
	a.goals.mu.Lock()
	service := a.goals.service
	closing := a.goals.closing
	a.goals.mu.Unlock()
	if service == nil || closing {
		return
	}
	switch event.Type {
	case "turn/finished":
		var result struct{ Status, Error string }
		if json.Unmarshal(event.Data, &result) != nil {
			service.Disarm(notice.SessionID)
			return
		}
		stale := strings.HasPrefix(result.Error, "GOAL_ROUND_STALE:") || strings.HasPrefix(result.Error, "GOAL_ROUND_YIELDED:")
		change, err := service.FinishTurn(notice.SessionID, event.TurnID, result.Status, stale)
		if err != nil {
			service.Disarm(notice.SessionID)
			return
		}
		if change != nil {
			a.notice(agent.Notice{SessionID: notice.SessionID, Type: "event", Data: *change})
		}
	case "goal/change":
	default:
		return
	}
	a.startGoalDriver(notice.SessionID)
}
func (a *App) startGoalDriver(id string) {
	if a.ctx == nil || a.engine == nil || a.ctx.Err() != nil {
		return
	}
	if _, foreground := a.foreground.Load(id); foreground {
		return
	}
	a.goals.mu.Lock()
	if a.goals.closing {
		a.goals.mu.Unlock()
		return
	}
	if a.goals.drivers == nil {
		a.goals.drivers = make(map[string]*goalDriver)
	}
	if a.goals.drivers[id] != nil {
		a.goals.drivers[id].requested = true
		a.goals.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(a.ctx)
	driver := &goalDriver{cancel: cancel, done: make(chan struct{})}
	a.goals.drivers[id] = driver
	a.goals.wg.Add(1)
	a.goals.mu.Unlock()
	go func() {
		defer close(driver.done)
		defer a.goals.wg.Done()
		defer cancel()
		defer func() {
			a.goals.mu.Lock()
			if a.goals.drivers[id] == driver {
				delete(a.goals.drivers, id)
			}
			a.goals.mu.Unlock()
		}()
		for {
			if err := a.engine.WaitIdle(ctx, id); err != nil {
				return
			}
			if _, foreground := a.foreground.Load(id); foreground {
				return
			}
			message, err := a.goalContinuation(ctx, id)
			if err != nil {
				a.goalService().Disarm(id)
				return
			}
			if message == nil {
				a.goals.mu.Lock()
				if driver.requested && !a.goals.closing {
					driver.requested = false
					a.goals.mu.Unlock()
					continue
				}
				if a.goals.drivers[id] == driver {
					delete(a.goals.drivers, id)
				}
				a.goals.mu.Unlock()
				return
			}
			err = a.engine.Run(ctx, id, *message)
			if err != nil {
				a.dropGoalReservation(id, message.DeliveryID)
				var failure *state.GoalError
				if errors.Is(err, agent.ErrBusy) {
					continue
				}
				if errors.As(err, &failure) && (failure.Code == "GOAL_ROUND_STALE" || failure.Code == "GOAL_ROUND_YIELDED") {
					continue
				}
				a.goalService().Disarm(id)
				return
			}
		}
	}()
}
func (a *App) waitGoalDrivers(ctx context.Context, id string) error {
	for {
		a.goals.mu.Lock()
		driver := a.goals.drivers[id]
		a.goals.mu.Unlock()
		if driver == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-driver.done:
		}
	}
}
func (a *App) closeGoals() {
	a.goals.mu.Lock()
	a.goals.closing = true
	if a.goals.service != nil {
		a.goals.service.DisarmAll()
	}
	for _, driver := range a.goals.drivers {
		driver.cancel()
	}
	a.goals.mu.Unlock()
	a.goals.wg.Wait()
}
