package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"dsgo/internal/core"
)

// InputBatchStore atomically persists one admission, keeping the direct input
// adjacent to its snapshots through partial-tail repair, restart and replay.
// The event is user/input with {version:1,message,contexts}. Existing stores and
// histories need no migration when preparation produces no sourced context.
type InputBatchStore interface {
	AppendInputBatch(id, turn string, message core.Message, contexts []core.Message) (core.Event, error)
}

// Legacy stores cannot expose batch positions. Known sourced references are
// never a direct turn boundary; modern stores answer from durable input events.
func isInputSnapshot(message core.Message) bool {
	var source struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(core.JSON(message.Source), &source); err != nil {
		return false
	}
	return source.Kind == "session-reference" || source.Kind == "file-reference"
}

func inputEventMessage(event core.Event) (core.Message, bool, error) {
	messages, recognized, err := core.UserInputMessages(event)
	if err != nil {
		return core.Message{}, recognized, fmt.Errorf("decode input event %d: %w", event.Seq, err)
	}
	if !recognized {
		return core.Message{}, false, nil
	}
	return messages[0], true, nil
}

func (e *Engine) inputAlreadyAccepted(id, delivery string) (bool, error) {
	events, ok := e.Store.(interface {
		Events(string) ([]core.Event, error)
	})
	if !ok {
		return false, errors.New("idempotent delivery requires the event store")
	}
	log, err := events.Events(id)
	if err != nil {
		return false, err
	}
	for _, event := range log {
		message, input, err := inputEventMessage(event)
		if err != nil {
			return false, err
		}
		if input && message.DeliveryID == delivery {
			return true, nil
		}
	}
	return false, nil
}

func (e *Engine) appendInput(ctx context.Context, id, turn string, message core.Message) (core.Message, error) {
	if err := ctx.Err(); err != nil {
		return core.Message{}, err
	}
	direct := message
	var contexts []core.Message
	if prepare := e.options().PrepareInput; prepare != nil {
		session, err := e.Store.Get(id)
		if err != nil {
			return core.Message{}, err
		}
		direct, contexts, err = prepare(ctx, session, message)
		if err != nil {
			return core.Message{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return core.Message{}, err
	}
	if direct.Role != "user" || len(direct.Content) == 0 || direct.DeliveryID != message.DeliveryID {
		return core.Message{}, errors.New("input preparation must preserve a nonempty user message and its delivery identity")
	}
	for _, snapshot := range contexts {
		if snapshot.Role != "user" || len(snapshot.Content) == 0 || snapshot.DeliveryID != "" {
			return core.Message{}, errors.New("input context must be a nonempty user snapshot without a delivery identity")
		}
	}
	var event core.Event
	var err error
	if commit := e.options().CommitPreparedInput; commit != nil {
		session, getErr := e.Store.Get(id)
		if getErr != nil {
			return core.Message{}, getErr
		}
		var handled bool
		event, handled, err = commit(ctx, session, turn, direct, contexts)
		if err != nil {
			return core.Message{}, err
		}
		if handled {
			e.emit(id, "event", event)
			return direct, nil
		}
	}
	if len(contexts) == 0 {
		event, err = e.Store.Append(id, turn, "user/message", direct)
	} else if store, ok := e.Store.(InputBatchStore); ok {
		// Never invoke PrepareInput recursively on snapshots. Their captured
		// bytes, including any quoted reference syntax, are already final.
		event, err = store.AppendInputBatch(id, turn, direct, contexts)
	} else {
		return core.Message{}, errors.New("input context requires atomic input-batch storage")
	}
	if err != nil {
		return core.Message{}, err
	}
	e.emit(id, "event", event)
	return direct, nil
}
