package agent

import (
	"context"
	"errors"

	"dsgo/internal/core"
)

// Automatic condensation follows the pinned compaction-basic defaults. With
// context window W and request output cap O, pressure starts at
// floor(min(W*0.8, W-O-65536)) and the newest 16% of W-O stays verbatim.
// Oversized tool results are trimmed first; summarizing runs only when the
// trimmed request still exceeds the threshold.
const (
	compactionThresholdRatio = 0.8
	compactionRetainRatio    = 0.16
	compactionHeadroom       = 65536
	compactionRetries        = 1
	maxOverflowRetries       = 1
)

// compactionSpec resolves the pressure threshold and verbatim budget. A
// window too small for the headroom falls back to the ratio alone, where the
// pinned plugin would refuse to load.
func compactionSpec(window, output int) (threshold, retain int, ok bool) {
	if window <= 0 {
		return 0, 0, false
	}
	threshold = int(float64(window) * compactionThresholdRatio)
	if room := window - output - compactionHeadroom; room > 0 {
		threshold = min(threshold, room)
	}
	retain = int(float64(max(window-output, 0)) * compactionRetainRatio)
	if retain >= threshold {
		retain = threshold / 2
	}
	return threshold, retain, threshold > 0
}

// relieveContextPressure measures the assembled request and, past the
// threshold, prunes tool results and condenses the oldest history. extras are
// the request's transient context messages, re-appended after each change.
func (e *Engine) relieveContextPressure(ctx context.Context, id, turn string, req core.Request, extras []core.Message) (core.Request, error) {
	route, err := e.contextRoute(ctx, req)
	if err != nil {
		return req, err
	}
	output := req.MaxTokens
	if output <= 0 {
		output = route.DefaultMaxTokens
	}
	threshold, retain, ok := compactionSpec(route.ContextWindow, output)
	if !ok {
		return req, nil
	}
	total, err := e.measuredTokens(ctx, id, req)
	if err != nil || total < threshold {
		return req, err
	}
	refresh := func() error {
		messages, err := e.Store.Messages(id)
		if err != nil {
			return err
		}
		req.Messages = append(messages, extras...)
		total, err = e.measuredTokens(ctx, id, req)
		return err
	}
	optionalFailure := func(cause error) (core.Request, error) {
		if ctx.Err() != nil {
			return req, ctx.Err()
		}
		if requiredCompactionFailure(cause) {
			return req, cause
		}
		// Earlier pruning may already have committed. Continue from that
		// current surface, never from the stale pre-maintenance request.
		if refreshErr := refresh(); refreshErr != nil {
			return req, errors.Join(cause, refreshErr)
		}
		e.compactionWarning(id, "context-pressure", "Context compaction failed; continuing with the current history.", cause)
		return req, nil
	}
	if pruned, err := e.pruneToolResults(ctx, id, turn); err != nil {
		return optionalFailure(err)
	} else if pruned > 0 {
		if err = refresh(); err != nil || total < threshold {
			return req, err
		}
	}
	for attempt := 0; attempt <= compactionRetries; attempt++ {
		changed, err := e.compactRetaining(ctx, id, turn, retain, req)
		if err != nil {
			return optionalFailure(err)
		}
		if !changed {
			return req, nil
		}
		if err = refresh(); err != nil || total < threshold {
			return req, err
		}
	}
	return req, nil
}

// measuredTokens prices the request through the token meter. A surface the
// meter cannot reconcile is not a reason to fail the step; it is skipped.
func (e *Engine) measuredTokens(ctx context.Context, id string, req core.Request) (int, error) {
	measurement, err := e.MeasureContext(ctx, id, req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, nil
	}
	return measurement.TotalTokens, nil
}

// recoverOverflow condenses as much as safely possible after the provider
// rejected a request for exceeding its context window.
func (e *Engine) recoverOverflow(ctx context.Context, id, turn string, target ...core.Request) (bool, error) {
	_, before, err := e.contextSurface(id)
	if err != nil {
		return false, err
	}
	_, workErr := e.pruneToolResults(ctx, id, turn)
	if workErr == nil {
		_, workErr = e.compactRetaining(ctx, id, turn, 0, target...)
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	_, after, readErr := e.contextSurface(id)
	if readErr != nil {
		return false, errors.Join(workErr, readErr)
	}
	return contextMutationRevision(after) > contextMutationRevision(before), workErr
}

func contextMutationRevision(events []core.Event) int {
	count := 0
	for _, event := range events {
		if event.Type == "context/replace" || event.Type == "context/compacted" {
			count++
		}
	}
	return count
}

func (e *Engine) compactionWarning(id, phase, message string, err error) {
	e.emit(id, "warning", map[string]any{"phase": phase, "message": message, "error": err.Error()})
}

// compactRetaining condenses the oldest history while keeping at least
// retain estimated tokens of the newest messages verbatim.
func (e *Engine) compactRetaining(ctx context.Context, id, turn string, retain int, target ...core.Request) (bool, error) {
	nodes, _, err := e.contextSurface(id)
	if err != nil {
		return false, err
	}
	cut, err := compactionCut(nodes, retain)
	if err != nil || cut <= 0 {
		return false, err
	}
	if err = e.compactAt(ctx, id, turn, core.ContextMessages(nodes), cut, target...); err != nil {
		return false, err
	}
	return true, nil
}

// compactionCut ports selectCompactableRange: accumulate from the newest
// message until the retained budget is met, then move the cut earlier until
// no tool call before it is waiting for its result. A dsgo input group (a
// direct input with its captured context) is never split.
func compactionCut(nodes []core.ContextNode, retain int) (int, error) {
	if len(nodes) == 0 {
		return 0, nil
	}
	keep, accumulated := len(nodes), 0
	for index := len(nodes) - 1; index >= 0; index-- {
		tokens, err := EstimateContextMessage(nodes[index].Message)
		if err != nil {
			return 0, err
		}
		accumulated += tokens
		keep = index
		if accumulated >= retain {
			break
		}
	}
	for keep > 0 && !balancedBefore(nodes, keep) {
		keep--
	}
	if keep == 1 && nodes[0].Message.Role == "system" {
		return 0, nil
	}
	return keep, nil
}

func balancedBefore(nodes []core.ContextNode, index int) bool {
	if group := nodes[index].InputGroup; group != 0 && nodes[index-1].InputGroup == group {
		return false
	}
	pending := map[string]bool{}
	for _, node := range nodes[:index] {
		for _, block := range node.Message.Content {
			switch block.Type {
			case "tool_use":
				pending[block.ID] = true
			case "tool_result":
				delete(pending, block.ToolUseID)
			}
		}
	}
	return len(pending) == 0
}

func isContextOverflow(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	return core.IsContextWindowExceeded(err.Error())
}
