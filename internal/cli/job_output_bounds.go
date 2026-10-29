package cli

import (
	"encoding/json"

	"dsgo/internal/core"
	"dsgo/internal/tools"
)

type jobOutputBoundKey struct {
	session, turn, execution, call, name string
}

type jobOutputBound struct {
	jobID string
	limit int
}

func jobBoundKey(tc core.ToolContext, name string) jobOutputBoundKey {
	return jobOutputBoundKey{tc.SessionID, tc.TurnID, tc.ExecutionID, tc.CallID, name}
}

// Capture before policy can remove a job or replace its result. ExecutionID is
// unique per top-level call and inherited by nested calls; CallID separates the
// nested dispatches within that execution.
func (a *App) beginJobOutputBound(tc core.ToolContext, name string, raw json.RawMessage) {
	if a.registry == nil || (name != "job_output" && name != "job_kill") {
		return
	}
	var input struct {
		JobID string `json:"job_id"`
	}
	if json.Unmarshal(raw, &input) != nil || input.JobID == "" {
		return
	}
	limit, _ := a.registry.JobOutputLimit(tc.SessionID, input.JobID)
	// Keep the argument for the pinned finalizer's visible-limit fallback.
	// A cap observed now always wins over a later registry lookup.
	a.jobOutputBounds.Store(jobBoundKey(tc, name), jobOutputBound{input.JobID, limit})
}

// A bounded error keeps cancellation and other typed causes available to the
// dispatcher while exposing the capped text to PTC's error projection.
type boundedJobError struct {
	cause error
	text  string
}

func (e *boundedJobError) Error() string { return e.text }
func (e *boundedJobError) Unwrap() error { return e.cause }

func (a *App) finishJobOutputBound(tc core.ToolContext, name string, result core.ToolResult, err error) (core.ToolResult, error) {
	value, ok := a.jobOutputBounds.LoadAndDelete(jobBoundKey(tc, name))
	if !ok {
		return result, err
	}
	bound := value.(jobOutputBound)
	limit := bound.limit
	if limit == 0 && a.registry != nil {
		limit, _ = a.registry.JobOutputLimit(tc.SessionID, bound.jobID)
	}
	if limit <= 0 {
		return result, err
	}
	// A structured policy replacement keeps its structure, as in tool-jobs'
	// rawSingleText check. Content is the text projection when Blocks is empty.
	text := result.Content
	if len(result.Blocks) > 0 {
		if len(result.Blocks) != 1 || result.Blocks[0].Type != "text" {
			return result, err
		}
		text = result.Blocks[0].Text
	}
	if err != nil {
		text = err.Error()
		result.IsError = true
	} else if !result.IsError && len(result.Blocks) == 0 && len(result.Value) > 0 && text == string(result.Value) {
		// Native calls render in spillToolResult. A PTC subcall bypasses that
		// renderer, so form its separate content projection before finalizing.
		if rendered, ok := tools.RenderShellResult(name, text); ok {
			text = rendered
		}
	}
	bounded := tools.BoundJobText(text, limit)
	if name == "job_output" && err == nil && !result.IsError {
		bounded = tools.BoundJobOutput(text, result.Value, limit)
	}
	result.Content = bounded
	if len(result.Blocks) == 1 {
		result.Blocks = append([]core.Block(nil), result.Blocks...)
		result.Blocks[0].Text = bounded
	}
	if err != nil && bounded != err.Error() {
		err = &boundedJobError{cause: err, text: bounded}
	}
	return result, err
}
