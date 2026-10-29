package tools

import (
	"context"
	"sync"
	"time"
)

// Retain holds a terminal for one physical client-window lifetime, independently
// of output attachments. Collapsing a pane does not release this hold.
func (c *TerminalController) Retain(ctx context.Context, owner, id string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	t, err := c.terminal(owner, id)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	if t.closing {
		t.mu.Unlock()
		return nil, terminalError("terminal/unavailable", "Terminal is closing")
	}
	t.nextHolder++
	holder := t.nextHolder
	t.holders[holder] = struct{}{}
	t.invalidateIdleLocked()
	t.mu.Unlock()
	holdCtx, cancel := context.WithCancel(ctx)
	stopTerminal := context.AfterFunc(t.ctx, cancel)
	var once sync.Once
	release := func() {
		once.Do(func() {
			stopTerminal()
			cancel()
			t.mu.Lock()
			delete(t.holders, holder)
			t.invalidateIdleLocked()
			t.mu.Unlock()
		})
	}
	stop := context.AfterFunc(holdCtx, release)
	return func() { stop(); release() }, nil
}
func (t *userTerminal) signalMonitorLocked() {
	select {
	case t.wakeup <- struct{}{}:
	default:
	}
}
func (c *TerminalController) monitor(owner, id string, t *userTerminal) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-c.lifetime.Done():
			return
		case <-t.ctx.Done():
			return
		case <-t.ownerDone:
			return
		case <-timer.C:
		case <-t.wakeup:
		}
		t.mu.Lock()
		closing, retry := t.closing, t.retryNeeded
		held := len(t.holders) > 0 || len(t.attachments) > 0
		epoch := t.activityEpoch
		t.mu.Unlock()
		if closing {
			if !retry {
				return
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(terminalDuration(t.options.CleanupRetryMS))
			select {
			case <-c.lifetime.Done():
				return
			case <-t.ctx.Done():
				return
			case <-t.ownerDone:
				return
			case <-timer.C:
			}
			if c.retire(owner, id, t, nil) == nil {
				return
			}
			continue
		}
		if held || *t.options.UnattendedTimeoutMS == 0 {
			continue
		}
		state, revision := "unknown", uint64(0)
		if inspector, ok := t.process.(interface {
			InspectActivity(context.Context) (string, uint64)
		}); ok {
			ctx, cancel := context.WithTimeout(t.ctx, terminalDuration(t.options.ActivityPollIntervalMS))
			state, revision = inspector.InspectActivity(ctx)
			cancel()
		}
		now := time.Now()
		t.mu.Lock()
		eligible := !t.closing && len(t.holders) == 0 && len(t.attachments) == 0 && epoch == t.activityEpoch
		if eligible {
			if state != "idle" {
				t.idleSince = time.Time{}
			} else {
				poll := terminalDuration(t.options.ActivityPollIntervalMS)
				if t.idleSince.IsZero() || revision != t.idleRevision || now.Sub(t.idleObserved) > poll*2 {
					t.idleSince = now
					t.idleRevision = revision
				}
				t.idleObserved = now
			}
		}
		due := eligible && !t.idleSince.IsZero() && now.Sub(t.idleSince) >= terminalDuration(*t.options.UnattendedTimeoutMS)
		t.mu.Unlock()
		if due {
			if c.retire(owner, id, t, &epoch) == nil {
				return
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(terminalDuration(t.options.ActivityPollIntervalMS))
	}
}
func (c *TerminalController) retire(owner, id string, t *userTerminal, epoch *uint64) error {
	c.mu.Lock()
	state := c.owners[owner]
	if state == nil || state.terminals[id] != t {
		c.mu.Unlock()
		return nil
	}
	t.mu.Lock()
	if epoch != nil && (t.closing || t.activityEpoch != *epoch || len(t.holders) > 0 || len(t.attachments) > 0) {
		t.mu.Unlock()
		c.mu.Unlock()
		return context.Canceled
	}
	t.closing = true
	t.accepting.Store(false)
	t.generation.Store(0)
	t.mu.Unlock()
	state.closedIDs[id] = true
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), max(5*time.Second, terminalDuration(t.options.DisposeGraceMS)*3))
	defer cancel()
	if err := t.close(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	if state.terminals[id] == t {
		delete(state.terminals, id)
	}
	c.mu.Unlock()
	return nil
}
