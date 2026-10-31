// Package tools implements dsgo's fixed built-in tool registry. It does not load plugins.
package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"

	"dsgo/internal/core"
	"dsgo/internal/jsonschema"
)

type Handler func(context.Context, core.ToolContext, json.RawMessage) (core.ToolResult, error)
type definition struct {
	spec                   core.ToolSpec
	run                    Handler
	inputValidatedByServer bool
}

// Registration belongs to one replaceable source of tool definitions.
type Registration struct {
	Spec core.ToolSpec
	Run  Handler
	// InputValidatedByServer is used only by the fixed MCP bridge. Its raw
	// schemas are forwarded to the model; the server owns argument validation.
	InputValidatedByServer bool
}

type Registry struct {
	mu             sync.RWMutex
	definitions    map[string]definition
	groups         map[string]map[string]bool
	jobs           map[string]*job
	observations   map[string]map[string]fileObservation
	spillDir       string
	notifier       func(JobNotice)
	closed         bool
	jobWaitDefault float64
	jobWaitCap     float64
}

func New() *Registry {
	r := &Registry{definitions: make(map[string]definition), groups: make(map[string]map[string]bool), jobs: make(map[string]*job), observations: make(map[string]map[string]fileObservation)}
	r.builtins()
	r.officialFiles()
	r.standardShell()
	return r
}

func (r *Registry) Register(spec core.ToolSpec, run Handler) error {
	if spec.Name == "" || run == nil || !json.Valid(spec.InputSchema) {
		return errors.New("invalid tool definition")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("tool registry closed")
	}
	if _, ok := r.definitions[spec.Name]; ok {
		return fmt.Errorf("tool %q already registered", spec.Name)
	}
	r.definitions[spec.Name] = definition{spec: spec, run: run}
	return nil
}

// ReplaceGroup publishes a complete discovery generation under one lock. An
// invalid discovery keeps the previous generation. A registration collision
// removes the old generation and publishes none of the attempted generation,
// matching the official MCP bridge's dispose/register/rollback boundary.
func (r *Registry) ReplaceGroup(group string, entries []Registration) error {
	if group == "" {
		return errors.New("tool group is required")
	}
	next := make(map[string]definition, len(entries))
	for _, entry := range entries {
		spec := entry.Spec
		if spec.Name == "" || entry.Run == nil {
			return errors.New("invalid tool definition")
		}
		if _, exists := next[spec.Name]; exists {
			return fmt.Errorf("duplicate tool %q in discovery", spec.Name)
		}
		if entry.InputValidatedByServer {
			if !json.Valid(spec.InputSchema) {
				return fmt.Errorf("tool %q has invalid schema JSON", spec.Name)
			}
		} else if _, err := jsonschema.Compile(spec.InputSchema); err != nil {
			return fmt.Errorf("tool %q schema: %w", spec.Name, err)
		}
		spec.InputSchema = bytes.Clone(spec.InputSchema)
		next[spec.Name] = definition{spec: spec, run: entry.Run, inputValidatedByServer: entry.InputValidatedByServer}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("tool registry closed")
	}
	for name := range r.groups[group] {
		delete(r.definitions, name)
	}
	delete(r.groups, group)
	for name := range next {
		if _, exists := r.definitions[name]; exists {
			return fmt.Errorf("tool %q already registered", name)
		}
	}
	if len(next) == 0 {
		return nil
	}
	names := make(map[string]bool, len(next))
	for name, d := range next {
		r.definitions[name] = d
		names[name] = true
	}
	if r.groups == nil {
		r.groups = make(map[string]map[string]bool)
	}
	r.groups[group] = names
	return nil
}

func (r *Registry) Specs() []core.ToolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]core.ToolSpec, 0, len(r.definitions))
	for _, d := range r.definitions {
		s := d.spec
		s.InputSchema = bytes.Clone(s.InputSchema)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Registry) Execute(ctx context.Context, tc core.ToolContext, name string, input json.RawMessage) (core.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return core.ToolResult{}, err
	}
	r.mu.RLock()
	d, ok := r.definitions[name]
	closed := r.closed
	r.mu.RUnlock()
	if closed {
		return core.ToolResult{}, errors.New("tool registry closed")
	}
	if !ok {
		return core.ToolResult{}, fmt.Errorf("unknown tool: %s", name)
	}
	if !d.inputValidatedByServer {
		if err := validate(d.spec.InputSchema, input); err != nil {
			return core.ToolResult{}, fmt.Errorf("%s: %w", name, err)
		}
	}
	return d.run(ctx, tc, bytes.Clone(input))
}

func (r *Registry) Close() error {
	r.mu.Lock()
	r.closed = true
	jobs := make([]*job, 0, len(r.jobs))
	for _, j := range r.jobs {
		jobs = append(jobs, j)
	}
	r.mu.Unlock()
	for _, j := range jobs {
		j.cancel()
	}
	for _, j := range jobs {
		<-j.done
	}
	return nil
}

// InvalidateSession must be called when a session's standing permission
// changes or closes. Each bash call starts a fresh shell, so no per-session
// process state is retained.
func (r *Registry) InvalidateSession(sessionID string) {}

func decode(input json.RawMessage, v any) error {
	d := json.NewDecoder(bytes.NewReader(input))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	var more any
	if err := d.Decode(&more); err != io.EOF {
		return errors.New("arguments must be one JSON object")
	}
	return nil
}

func validate(raw, jsonInput []byte) error {
	return jsonschema.Validate(raw, jsonInput)
}

func spec(name, description, raw string) core.ToolSpec {
	return core.ToolSpec{Name: name, Description: description, InputSchema: json.RawMessage(raw)}
}
func result(v any) core.ToolResult {
	b, _ := json.Marshal(v)
	return core.ToolResult{Content: string(b)}
}
func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func ask(ctx context.Context, tc core.ToolContext, name, description string, input json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a := core.Approval{ID: newID(), SessionID: tc.SessionID, Tool: name, Description: description, Input: input}
	if tc.Emit != nil {
		if err := tc.Emit("approval/requested", a); err != nil {
			return fmt.Errorf("approval request audit failed: %w", err)
		}
	}
	allowed := false
	var err error
	if tc.ApprovalPolicy == "never" {
		err = errors.New("approval policy is never")
	} else if tc.Approve == nil {
		err = errors.New("approval channel unavailable")
	} else {
		allowed, err = tc.Approve(ctx, a)
	}
	if ctx.Err() != nil {
		allowed = false
		err = ctx.Err()
	}
	if tc.Emit != nil {
		if emitErr := tc.Emit("approval/settled", map[string]any{"id": a.ID, "allowed": allowed, "error": errorString(err)}); emitErr != nil {
			return fmt.Errorf("approval decision audit failed: %w", emitErr)
		}
	}
	if err != nil {
		return fmt.Errorf("permission denied: %w", err)
	}
	if !allowed {
		return errUserDenied
	}
	return nil
}

var errUserDenied = errors.New("permission denied by user")

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
