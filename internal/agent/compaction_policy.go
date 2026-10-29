package agent

import (
	"context"
	"errors"
	"regexp"

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
	if pruned, err := e.pruneToolResults(ctx, id, turn); err != nil {
		return req, err
	} else if pruned > 0 {
		if err = refresh(); err != nil || total < threshold {
			return req, err
		}
	}
	for attempt := 0; attempt <= compactionRetries; attempt++ {
		changed, err := e.compactRetaining(ctx, id, turn, retain)
		if err != nil || !changed {
			return req, err
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
func (e *Engine) recoverOverflow(ctx context.Context, id, turn string) (bool, error) {
	pruned, err := e.pruneToolResults(ctx, id, turn)
	if err != nil {
		return false, err
	}
	compacted, err := e.compactRetaining(ctx, id, turn, 0)
	return pruned > 0 || compacted, err
}

// compactRetaining condenses the oldest history while keeping at least
// retain estimated tokens of the newest messages verbatim.
func (e *Engine) compactRetaining(ctx context.Context, id, turn string, retain int) (bool, error) {
	nodes, _, err := e.contextSurface(id)
	if err != nil {
		return false, err
	}
	cut, err := compactionCut(nodes, retain)
	if err != nil || cut <= 0 {
		return false, err
	}
	if err = e.compactAt(ctx, id, turn, core.ContextMessages(nodes), cut); err != nil {
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

// The provider error dialects the pinned llm package recognizes as a
// context-window overflow.
var contextOverflowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:^|[^a-z0-9])context[\s_-](?:length|window)[\s_-](?:exceed(?:ed|s)?|overflow(?:ed)?|limit[\s_-]exceeded)(?:$|[^a-z0-9])`),
	regexp.MustCompile(`(?i)\b(?:maximum|max)(?:\s+(?:allowed|supported))?\s+context\s+(?:length|window)\b`),
	regexp.MustCompile(`(?i)\b(?:request|prompt|input|messages?)\s+(?:is\s+|are\s+)?too\s+(?:large|long)\s+for\s+(?:(?:this|the)\s+)?(?:model(?:'s)?\s+)?context(?:\s+window)?\b`),
	regexp.MustCompile(`(?i)\b(?:input|prompt|request)\s+(?:is\s+)?too\s+(?:long|large)\s+for\s+(?:this|the)\s+model\b`),
	regexp.MustCompile(`(?i)\b(?:input|prompt|request|messages?)\b.{0,40}\b(?:exceed(?:s|ed)?|overflows?|is\s+larger\s+than)\b.{0,40}\b(?:the\s+)?(?:model(?:'s)?\s+)?context(?:\s+(?:length|window))?\b`),
}

func isContextOverflow(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	for _, pattern := range contextOverflowPatterns {
		if pattern.MatchString(err.Error()) {
			return true
		}
	}
	return false
}
