package cli

import (
	"encoding/json"

	"dsgo/internal/agent"
	"dsgo/internal/core"
)

type jobWakeCount struct {
	owner *child
	spent int
}

func (a *App) configureJobs() error {
	defaultMS, maximumMS := 30000.0, 600000.0
	if cfg := a.cfg.Jobs; cfg != nil {
		if cfg.WaitTimeoutMS != nil {
			defaultMS = *cfg.WaitTimeoutMS
		}
		if cfg.MaxWaitTimeoutMS != nil {
			maximumMS = *cfg.MaxWaitTimeoutMS
		}
	}
	return a.registry.SetJobWait(defaultMS, maximumMS)
}

func (a *App) resetJobWakeBudget(id string) {
	a.jobWakeMu.Lock()
	delete(a.jobWakes, id)
	a.jobWakeMu.Unlock()
}

func (a *App) jobInputAccepted(notice agent.Notice) {
	if notice.Type != "event" {
		return
	}
	event, ok := notice.Data.(core.Event)
	if !ok {
		return
	}
	messages, recognized, err := core.UserInputMessages(event)
	if !recognized || err != nil || len(messages) == 0 {
		return
	}
	message := messages[0]
	if message.Source != nil {
		var source struct{ Kind string }
		if json.Unmarshal(core.JSON(message.Source), &source) != nil || source.Kind != "user" {
			return
		}
	}
	a.resetJobWakeBudget(notice.SessionID)
}

// Completion decisions are serialized just like the Host's settlement
// listener. Queued wake ownership is visible before releasing the lock, so
// simultaneous notices share one activation and spend one wake at most.
func (a *App) deliverJobCompletion(owner string, message core.Message) (string, error) {
	a.jobWakeMu.Lock()
	defer a.jobWakeMu.Unlock()
	manager := a.manager
	manager.mu.Lock()
	if manager.closing {
		manager.mu.Unlock()
		return "", errManagerClosing
	}
	if manager.isDrainingLocked(owner) {
		manager.mu.Unlock()
		return "", errManagerClosing
	}
	identity := manager.children[owner]
	if identity != nil && identity.torndown {
		manager.mu.Unlock()
		return "", errManagerClosing
	}
	busy, paused := false, manager.wakePaused[owner]
	if identity != nil {
		busy = identity.Status == "running"
		paused = paused || identity.torndown
	}
	for _, wake := range manager.wakeRuns {
		busy = busy || wake.target == owner
	}
	manager.mu.Unlock()
	busy = busy || a.engine.Busy(owner)
	wake := !paused
	if cfg := a.cfg.Jobs; cfg != nil {
		if cfg.CompletionDelivery == "quiet" {
			wake = false
		}
		if cfg.MaxConsecutiveWakes != nil && wake && !busy {
			count := a.jobWakes[owner]
			if count.owner != identity {
				count = jobWakeCount{owner: identity}
			}
			if count.spent >= *cfg.MaxConsecutiveWakes {
				wake = false
			} else {
				count.spent++
				if a.jobWakes == nil {
					a.jobWakes = map[string]jobWakeCount{}
				}
				a.jobWakes[owner] = count
			}
		}
	}
	if !wake {
		return a.engine.Inbox(a.ctx, owner, message)
	}
	return manager.deliver(a.ctx, owner, message, true)
}
