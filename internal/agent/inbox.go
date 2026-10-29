package agent

import (
	"context"
	"encoding/json"
	"errors"

	"dsgo/internal/core"
)

type inboxEntry struct {
	ID      string       `json:"id"`
	Message core.Message `json:"message"`
}

var ErrBusy = errors.New("already has a running turn")

// WaitIdle waits for the currently published turn's complete cleanup. It does
// not create work and does not wait for unrelated sessions.
func (e *Engine) WaitIdle(ctx context.Context, id string) error {
	e.mu.Lock()
	done := e.idle[id]
	e.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ContinueInbox is a managed wake-up for messages racing a turn's final
// boundary. It waits for the owner instead of starting overlapping turns.
func (e *Engine) ContinueInbox(ctx context.Context, id string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		e.mu.Lock()
		done := e.idle[id]
		closed := e.closed
		e.mu.Unlock()
		if closed {
			return errors.New("agent engine is closed")
		}
		if done != nil {
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		pending, err := e.PendingInbox(id)
		if err != nil || !pending {
			return err
		}
		err = e.Continue(ctx, id)
		if errors.Is(err, ErrBusy) {
			continue
		}
		if err != nil {
			return err
		}
	}
}

// Inbox commits a live steering message before acknowledging it. Only the turn
// owner projects queued messages, at a complete tool-result boundary.
func (e *Engine) Inbox(ctx context.Context, id string, message core.Message) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if message.Role != "user" || len(message.Content) == 0 {
		return "", errors.New("inbox requires a nonempty user message")
	}
	e.mu.Lock()
	closed := e.closed
	e.mu.Unlock()
	if closed {
		return "", errors.New("agent engine is closed")
	}
	if _, err := e.Store.Get(id); err != nil {
		return "", err
	}
	unlockInput := func() {}
	if structured := e.structuredRun(id); structured != nil {
		structured.mu.Lock()
		if !structured.acceptsInputLocked() {
			structured.mu.Unlock()
			return "", errors.New("structured output already staged: input is closed")
		}
		unlockInput = structured.mu.Unlock
	}
	entry := inboxEntry{ID: ID(), Message: message}
	event, err := e.Store.Append(id, "", "user/inbox", entry)
	unlockInput()
	if err != nil {
		return "", err
	}
	e.emit(id, "event", event)
	return entry.ID, nil
}

func (e *Engine) pendingInbox(id string) ([]inboxEntry, error) {
	store, ok := e.Store.(interface {
		Events(string) ([]core.Event, error)
	})
	if !ok {
		return nil, nil
	}
	events, err := store.Events(id)
	if err != nil {
		return nil, err
	}
	delivered := map[string]bool{}
	var pending []inboxEntry
	for _, event := range events {
		switch event.Type {
		case "user/message", "user/input":
			message, _, err := inputEventMessage(event)
			if err != nil {
				return nil, err
			}
			if message.DeliveryID != "" {
				delivered[message.DeliveryID] = true
			}
		case "user/inbox":
			var entry inboxEntry
			if err := json.Unmarshal(event.Data, &entry); err != nil {
				return nil, err
			}
			if entry.ID == "" || entry.Message.Role != "user" {
				return nil, errors.New("invalid persisted inbox entry")
			}
			pending = append(pending, entry)
		}
	}
	out := pending[:0]
	for _, entry := range pending {
		if !delivered["inbox:"+entry.ID] {
			out = append(out, entry)
		}
	}
	return out, nil
}

func (e *Engine) PendingInbox(id string) (bool, error) {
	entries, err := e.pendingInbox(id)
	return len(entries) > 0, err
}

func (e *Engine) admitInbox(ctx context.Context, id, turn string) (bool, error) {
	entries, err := e.pendingInbox(id)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		message := entry.Message
		message.DeliveryID = "inbox:" + entry.ID
		_, err := e.appendInput(ctx, id, turn, message)
		if err != nil {
			return false, err
		}
	}
	return len(entries) > 0, nil
}
