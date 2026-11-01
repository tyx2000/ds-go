package cli

import (
	"encoding/json"
	"errors"
	"fmt"

	"dsgo/internal/core"
)

// Only top-level managed wakes publish these outcomes. A child's failure is
// instead delivered to its parent, which may recover and finish successfully.
type rootWakeFailure struct {
	id          uint64
	afterSeq    int64
	tracked     bool
	err         error
	beforeSeq   int64
	beforeKnown bool
	cause       error
}

func (m *agents) wakeStartSequence(target string) (int64, bool) {
	events, err := m.app.store.Events(target)
	if err != nil {
		return 0, false
	}
	if len(events) == 0 {
		return 0, true
	}
	return events[len(events)-1].Seq, true
}

type wakeTurnFinish struct{ Status, Error string }

// The failed turn is the recovery boundary when it was persisted. Errors
// before a turn was started (or after its audit failed) use the latest visible
// event instead: only a later real success can resolve them. A no-op wake that
// merely waited for another owner must never turn a failure into success.
func wakeFailureBoundary(failure rootWakeFailure, events []core.Event) rootWakeFailure {
	if failure.tracked {
		return failure
	}
	failure.tracked = true
	if len(events) > 0 {
		failure.afterSeq = events[len(events)-1].Seq
	}
	if failure.beforeKnown && failure.cause != nil {
		for _, event := range events {
			if event.Seq <= failure.beforeSeq || event.Type != "turn/finished" {
				continue
			}
			var finish wakeTurnFinish
			if json.Unmarshal(event.Data, &finish) == nil && finish.Status != "completed" && finish.Error == failure.cause.Error() {
				failure.afterSeq = event.Seq
			}
		}
	}
	return failure
}

func (m *agents) recordWakeFailure(target string, id uint64, before int64, known bool, cause error) {
	failure := rootWakeFailure{id: id, err: cause, beforeSeq: before, beforeKnown: known, cause: cause}
	events, err := m.app.store.Events(target)
	if err != nil {
		failure.err = errors.Join(cause, fmt.Errorf("read automatic turn outcome: %w", err))
	} else {
		failure = wakeFailureBoundary(failure, events)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.wakeFailures == nil {
		m.wakeFailures = make(map[string]rootWakeFailure)
	}
	previous, exists := m.wakeFailures[target]
	if exists && !previous.tracked && err == nil {
		// A temporary metadata read failure must not become a permanent latch.
		// Recover its exact failed finish when possible; otherwise establish
		// a conservative anchor now, before crediting any future success.
		previous = wakeFailureBoundary(previous, events)
		m.wakeFailures[target] = previous
	}
	if exists && !failure.tracked {
		// Several unreadable outcomes have no provable relative finish order.
		// Keep all causes and resolve them from a fresh tail once reads recover.
		failure.err = errors.Join(previous.err, failure.err)
		failure.beforeKnown, failure.cause = false, nil
	}
	if !exists || !failure.tracked || (previous.tracked && failure.afterSeq >= previous.afterSeq) {
		m.wakeFailures[target] = failure
	}
}

func latestSuccessfulWakeTurn(events []core.Event) int64 {
	var success int64
	for _, event := range events {
		if event.Type != "turn/finished" {
			continue
		}
		var finish wakeTurnFinish
		if json.Unmarshal(event.Data, &finish) == nil && finish.Status == "completed" {
			success = max(success, event.Seq)
		}
	}
	return success
}

// Called only after owned work is quiescent. Keep root failures visible to the
// one-shot caller until an actual later turn has successfully recovered them.
func (m *agents) rootWakeError(target string) error {
	m.mu.Lock()
	failure, exists := m.wakeFailures[target]
	m.mu.Unlock()
	if !exists {
		return nil
	}
	events, err := m.app.store.Events(target)
	if err != nil {
		return errors.Join(failure.err, err)
	}
	failure = wakeFailureBoundary(failure, events)
	if latestSuccessfulWakeTurn(events) > failure.afterSeq {
		m.mu.Lock()
		current, present := m.wakeFailures[target]
		if !present {
			m.mu.Unlock()
			return nil
		}
		if current.id == failure.id {
			delete(m.wakeFailures, target)
			m.mu.Unlock()
			return nil
		}
		m.mu.Unlock()
		return fmt.Errorf("automatic turn for %s: %w", target, current.err)
	}
	m.mu.Lock()
	if current, present := m.wakeFailures[target]; present && current.id == failure.id {
		m.wakeFailures[target] = failure
	}
	m.mu.Unlock()
	return fmt.Errorf("automatic turn for %s: %w", target, failure.err)
}
