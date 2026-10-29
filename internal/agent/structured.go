package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"dsgo/internal/core"
	"dsgo/internal/jsonschema"
)

const StructuredOutputTool = "structured_output"
const structuredOutputInstruction = "When you have your final answer, you MUST report it by calling the `structured_output` tool with arguments matching its parameter schema exactly. Do not finish with a plain text answer: only the tool call counts as your result."

type structuredExecution struct{ execution, call string }
type structuredStage struct {
	value  json.RawMessage
	nested bool
}
type structuredPending struct {
	execution string
	value     json.RawMessage
}

// StructuredRun is activation-local. Nothing in this handle becomes resume
// configuration: releasing it removes the tool, instruction, and terminal guard.
// Its value commits only after the authoritative result, including the outer PTC
// result for a nested submission, succeeds.
type StructuredRun struct {
	mu          sync.Mutex
	spec        core.ToolSpec
	schema      *jsonschema.Schema
	canComplete func() bool
	staged      map[structuredExecution]structuredStage
	active      map[structuredExecution]bool // bool identifies a nested dispatch
	pending     *structuredPending
	captured    json.RawMessage
	injected    int
	released    bool
}

func (e *Engine) structuredRun(id string) *StructuredRun {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.structured[id]
}

func (e *Engine) AttachStructured(id string, schema json.RawMessage, canComplete func() bool) (*StructuredRun, func(), error) {
	var root struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(schema, &root); err != nil || root.Type != "object" {
		return nil, nil, errors.New("structured output requires an object-rooted JSON schema")
	}
	compiled, err := jsonschema.Compile(schema)
	if err != nil {
		return nil, nil, err
	}
	if _, err := e.Store.Get(id); err != nil {
		return nil, nil, err
	}
	run := &StructuredRun{
		spec:   core.ToolSpec{Name: StructuredOutputTool, Description: "Report your final structured result. Call this exactly once, when your answer is complete; the arguments must match this tool's parameter schema exactly.", InputSchema: bytes.Clone(schema)},
		schema: compiled, canComplete: canComplete,
		staged: make(map[structuredExecution]structuredStage), active: make(map[structuredExecution]bool),
	}
	e.mu.Lock()
	if e.closed || e.running[id] != nil || e.structured[id] != nil {
		e.mu.Unlock()
		return nil, nil, errors.New("structured output can only attach to an idle session without an existing attachment")
	}
	if e.structured == nil {
		e.structured = make(map[string]*StructuredRun)
	}
	e.structured[id] = run
	e.mu.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			e.mu.Lock()
			if e.structured[id] == run {
				delete(e.structured, id)
			}
			e.mu.Unlock()
			run.mu.Lock()
			run.released = true
			clear(run.staged)
			clear(run.active)
			run.pending = nil
			run.mu.Unlock()
		})
	}
	return run, release, nil
}

func (e *Engine) StructuredSpec(id string) (core.ToolSpec, bool) {
	run := e.structuredRun(id)
	if run == nil {
		return core.ToolSpec{}, false
	}
	spec := run.spec
	spec.InputSchema = bytes.Clone(spec.InputSchema)
	return spec, true
}

func (e *Engine) StructuredInstruction(id string) string {
	_, ok := e.StructuredSpec(id)
	if !ok {
		return ""
	}
	return structuredOutputInstruction
}

func withStructuredSpec(specs []core.ToolSpec, scoped core.ToolSpec) []core.ToolSpec {
	out := make([]core.ToolSpec, 0, len(specs)+1)
	for _, spec := range specs {
		if spec.Name != scoped.Name {
			out = append(out, spec)
		}
	}
	return append(out, scoped)
}

func (r *StructuredRun) acceptsInputLocked() bool {
	return !r.released && len(r.staged) == 0 && r.pending == nil && r.captured == nil
}
func (r *StructuredRun) AcceptsInput() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.acceptsInputLocked()
}
func (r *StructuredRun) Captured() (json.RawMessage, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return bytes.Clone(r.captured), r.captured != nil
}
func (r *StructuredRun) acceptInjected() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.acceptsInputLocked() {
		return errors.New("structured output already staged: input is closed")
	}
	r.injected++
	return nil
}
func (r *StructuredRun) consumeInjected(count int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.injected = max(0, r.injected-count)
}
func (r *StructuredRun) abortUncommitted() {
	r.mu.Lock()
	defer r.mu.Unlock()
	clear(r.staged)
	clear(r.active)
	r.pending = nil
	r.injected = 0
}

// ExecuteStructured must run before every top-level and nested tool dispatch.
// Ordinary calls are registered until their authoritative result so a parallel
// submission cannot overtake a child start that has not reserved its slot yet.
func (e *Engine) ExecuteStructured(ctx context.Context, tc core.ToolContext, name string, raw json.RawMessage) (core.ToolResult, bool, error) {
	run := e.structuredRun(tc.SessionID)
	if run == nil {
		return core.ToolResult{}, false, nil
	}
	if err := ctx.Err(); err != nil {
		return core.ToolResult{}, true, err
	}
	key := structuredExecution{tc.ExecutionID, tc.CallID}
	run.mu.Lock()
	if !run.acceptsInputLocked() {
		run.mu.Unlock()
		return core.ToolResult{}, true, fmt.Errorf("structured output already recorded or staged: the run is complete, so %q is not executed", name)
	}
	if tc.ExecutionID == "" || tc.CallID == "" {
		run.mu.Unlock()
		return core.ToolResult{}, true, errors.New("structured execution requires an execution identity")
	}
	if name != StructuredOutputTool {
		run.active[key] = tc.Nested
		run.mu.Unlock()
		return core.ToolResult{}, false, nil
	}
	run.mu.Unlock()
	if err := run.schema.ValidateJSON(raw); err != nil {
		return core.ToolResult{}, true, fmt.Errorf("INVALID_ARGS: %w", err)
	}
	// Never hold the input/capture lock while asking the manager about owned
	// children: delivery holds its manager lock before entering Engine.Inbox.
	if run.canComplete != nil && !run.canComplete() {
		return core.ToolResult{}, true, errors.New("Wait for all delegated child tasks to finish and process pending messages before submitting structured output.")
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if !run.acceptsInputLocked() {
		return core.ToolResult{}, true, errors.New("structured output already staged")
	}
	for active, nested := range run.active {
		if !nested && active.execution == tc.ExecutionID && tc.Nested {
			continue
		}
		return core.ToolResult{}, true, errors.New("Wait for all running tool calls to finish before submitting structured output.")
	}
	if run.injected != 0 {
		return core.ToolResult{}, true, errors.New("Process pending messages before submitting structured output.")
	}
	if pending, err := e.PendingInbox(tc.SessionID); err != nil {
		return core.ToolResult{}, true, err
	} else if pending {
		return core.ToolResult{}, true, errors.New("Process pending messages before submitting structured output.")
	}
	run.staged[key] = structuredStage{value: bytes.Clone(raw), nested: tc.Nested}
	return core.ToolResult{Content: "Structured output recorded.", Value: json.RawMessage(`{"recorded":true}`)}, true, nil
}

func (e *Engine) structuredSubresult(id, execution string, data any) {
	run := e.structuredRun(id)
	if run == nil {
		return
	}
	var value struct {
		CallID string          `json:"call_id"`
		Result core.ToolResult `json:"result"`
		Error  string          `json:"error"`
	}
	if err := json.Unmarshal(core.JSON(data), &value); err != nil || value.CallID == "" {
		return
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	key := structuredExecution{execution, value.CallID}
	delete(run.active, key)
	stage, ok := run.staged[key]
	delete(run.staged, key)
	if !ok || !stage.nested || value.Result.IsError || value.Error != "" || run.released {
		return
	}
	if run.captured == nil && run.pending == nil {
		run.pending = &structuredPending{execution: execution, value: stage.value}
	}
}

func (e *Engine) structuredTopResult(id, execution, call string, success bool) {
	run := e.structuredRun(id)
	if run == nil {
		return
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	key := structuredExecution{execution, call}
	if stage, ok := run.staged[key]; ok && !stage.nested && success && !run.released && run.captured == nil {
		run.captured = stage.value
	}
	if run.pending != nil && run.pending.execution == execution {
		if success && !run.released && run.captured == nil {
			run.captured = run.pending.value
		}
		run.pending = nil
	}
	for staged := range run.staged {
		if staged.execution == execution {
			delete(run.staged, staged)
		}
	}
	for active := range run.active {
		if active.execution == execution {
			delete(run.active, active)
		}
	}
}
