package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"dsgo/internal/core"
)

// The pinned summarizer replays the conversation before this final directive.
// It is ordinary request content, never a JSON transcript or a new system role.
const compactionInstruction = `You are now acting as a compaction engine for this AI coding assistant. Condense the conversation ABOVE into a structured checkpoint that lets another model resume the work with no loss of essential context.

Output EXACTLY the Markdown structure below: keep every section, in order. Use terse bullets, not prose paragraphs. Write "(none)" for an empty section — never drop a section.

## Primary Request and Intent
- [the user's original and evolving goals; quote verbatim where the exact wording matters]

## Key Technical Concepts
- [technologies, frameworks, patterns, and conventions in play]

## Files and Code
- [exact path: why it matters, key changes or snippets]

## Errors and Fixes
- [error: how it was resolved, plus any related user feedback]

## Pending Jobs
- [explicitly requested work not yet completed]

## Current Work
- [precisely what was in progress at this checkpoint]

## Next Step
- [the single next action, directly in line with the most recent request, or "(none)"]

## Critical Context
- [decisions and their rationale, constraints, user preferences, open questions, data needed to continue]

Rules:
- Write concise English engineering prose. Preserve exact file paths, commands, error strings, identifiers, numeric values, function signatures, and syntax fragments.
- Capture user feedback and explicit instructions faithfully, especially corrections.
- Do NOT mention this summarization request or that the context was compacted.
- Output only the checkpoint text: do not call any tool or take any other action.
- If the conversation already contains a <compacted-summary> block, it is a PRIOR checkpoint. Do not copy it forward verbatim: preserve still-true facts, drop stale ones, and merge newer information into a single consolidated summary under the same structure.`

const checkpointPreamble = "This is an automatically generated checkpoint condensing an earlier span of the conversation to free up context. Treat the captured context as established background and build on it without restating it. Continue the task directly from the messages that follow, without acknowledging this checkpoint."

// A required post-commit producer (for example the instruction baseline) must
// not be weakened into the optional summarizer's warn-and-continue behavior.
type compactionPostCommitError struct{ error }

func (e *compactionPostCommitError) Unwrap() error { return e.error }
func requiredCompactionFailure(err error) bool {
	var required *compactionPostCommitError
	return errors.As(err, &required) || errors.Is(err, context.Canceled)
}

// compactAt replaces only the selected prefix, preserving its system head and
// all retained origins/groups. The complete surface participates in the CAS.
func (e *Engine) compactAt(ctx context.Context, id, turn string, messages []core.Message, cut int, target ...core.Request) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store, ok := e.Store.(contextReplacementStore)
	if !ok {
		return errors.New("durable compaction requires atomic context replacement")
	}
	nodes, events, err := e.contextSurface(id)
	if err != nil {
		return err
	}
	if len(nodes) != len(messages) || cut < 1 || cut > len(nodes) {
		return errors.New("compaction surface changed before summarization")
	}
	for i, node := range nodes {
		if !bytes.Equal(core.JSON(node.Message), core.JSON(messages[i])) {
			return errors.New("compaction surface changed before summarization")
		}
	}
	start := 0
	if nodes[0].Message.Role == "system" {
		start = 1
	}
	if cut <= start {
		return errors.New("no non-system history prefix available for compaction")
	}
	if (start > 0 && !balancedBefore(nodes, start)) || (cut < len(nodes) && !balancedBefore(nodes, cut)) {
		return errors.New("compaction cannot split an input batch or pending tool call")
	}
	request, err := e.compactionRequest(ctx, id, events, target)
	if err != nil {
		return err
	}
	route, err := e.contextRoute(ctx, request)
	if err != nil {
		return err
	}
	prices, err := priceContextMessages(core.ContextMessages(nodes[start:cut]), route)
	if err != nil {
		return err
	}
	shadowed := 0
	for _, price := range prices {
		shadowed += price
	}
	request.Messages = append(compactionHistory(messages[:cut]), core.TextMessage("user", compactionInstruction))
	request.SessionID, request.Purpose = id, "compaction"
	request.MaxTokens, request.Thinking = compactionHeadroom, "off"
	response, err := e.Model.Stream(ctx, request, nil)
	if err != nil {
		return fmt.Errorf("context compaction: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	summary, err := compactionSummary(response)
	if err != nil {
		return err
	}
	checkpoint := core.Message{Role: "user", Source: map[string]any{"kind": "compact-checkpoint", "compactionId": ID()}, Content: append([]core.Block{{Type: "text", Text: checkpointPreamble + "\n\n<compacted-summary>"}}, summary...)}
	checkpoint.Content = append(checkpoint.Content, core.Block{Type: "text", Text: "</compacted-summary>"})
	price, err := EstimateContextMessage(checkpoint)
	if err != nil {
		return err
	}
	if price >= shadowed {
		return fmt.Errorf("compaction summary is not smaller than its source (%d >= %d tokens)", price, shadowed)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	event, err := store.AppendContextReplacement(id, turn, core.ContextReplacement{Version: 1, Expected: core.ContextOrigins(nodes), Start: start, End: cut, Messages: []core.Message{checkpoint}})
	committed := err == nil
	if !committed && event.Seq != 0 {
		// Append can report a later session-metadata failure after fsync.
		// Verify this exact checkpoint before restoring required baselines.
		current, _, readErr := e.contextSurface(id)
		if readErr == nil {
			for _, node := range current {
				if node.Origin.Seq == event.Seq && bytes.Equal(core.JSON(node.Message), core.JSON(checkpoint)) {
					committed = true
					break
				}
			}
		}
	}
	if committed && event.Seq != 0 {
		e.emit(id, "event", event)
	}
	if !committed {
		return err
	}
	if callback := e.options().AfterCompaction; callback != nil {
		session, err := e.Store.Get(id)
		if err != nil {
			return &compactionPostCommitError{err}
		}
		if err = callback(ctx, session, turn); err != nil {
			return &compactionPostCommitError{err}
		}
	}
	return err
}

func compactionSummary(response core.Response) ([]core.Block, error) {
	if response.StopReason == "max_tokens" || response.StopReason == "model_context_window_exceeded" {
		return nil, errors.New("summarization truncated at the token cap (incomplete checkpoint)")
	}
	var summary []core.Block
	nonempty := false
	for _, block := range response.Content {
		if block.Type == "image" {
			return nil, errors.New("compaction summary cannot contain image output")
		}
		if block.Type != "text" {
			continue
		}
		summary = append(summary, core.Block{Type: "text", Text: block.Text})
		nonempty = nonempty || strings.TrimSpace(block.Text) != ""
	}
	if !nonempty {
		return nil, errors.New("summarization produced no text summary content")
	}
	return summary, nil
}

// Use the actual routed envelope for automatic work. Manual work replays the
// last logged system/tools, falling back to the current fixed composition.
func (e *Engine) compactionRequest(ctx context.Context, id string, events []core.Event, target []core.Request) (core.Request, error) {
	if len(target) > 0 {
		request := target[0]
		request.Messages = nil
		request.ReplayIdentity = ""
		return request, nil
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != "context/request" {
			continue
		}
		var record contextRequestRecord
		if json.Unmarshal(events[i].Data, &record) != nil || record.Version != 1 {
			return core.Request{}, errors.New("invalid compaction request header")
		}
		request := core.Request{Provider: record.Header.Provider, Model: record.Header.Model, System: record.System, SystemPromptUpdate: record.Header.SystemPromptUpdate, ToolUpdate: record.Header.ToolUpdate}
		for _, tool := range record.Header.Tools {
			request.Tools = append(request.Tools, core.ToolSpec{Name: tool.Name, Description: tool.Description, InputSchema: core.JSON(tool.Parameters), DeferLoading: tool.DeferLoading})
		}
		return request, nil
	}
	session, err := e.Store.Get(id)
	if err != nil {
		return core.Request{}, err
	}
	opts := e.options()
	request := core.Request{Provider: session.Provider, Model: session.Model, System: opts.System}
	if request.Provider == "" {
		request.Provider = opts.Provider
	}
	if request.Model == "" {
		request.Model = opts.Model
	}
	if opts.SystemFor != nil {
		request.System, err = opts.SystemFor(session)
		if err != nil {
			return core.Request{}, err
		}
	}
	if opts.ToolsFor != nil {
		request.Tools = opts.ToolsFor(session)
	} else if e.Tools != nil {
		request.Tools = e.Tools.Specs()
	}

	return request, ctx.Err()
}
