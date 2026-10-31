// Package agent owns turn execution. Only settled assistant messages enter the
// next request; failed and cancelled attempts remain in the audit log.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"dsgo/internal/core"
)

type Store interface {
	Get(string) (core.Session, error)
	Update(core.Session) error
	Append(string, string, string, any) (core.Event, error)
	Messages(string) ([]core.Message, error)
}

type Options struct {
	Provider         string
	Model            string
	Thinking         string
	MaxTokens        int
	MaxSteps         int
	ContextTokens    int
	ContextFor       func(context.Context, core.Request) (ContextRoute, error)
	ToolResultPruner *ToolResultPruneConfig
	ApprovalPolicy   string
	System           string
	SystemFor        func(core.Session) (string, error)
	ToolsFor         func(core.Session) []core.ToolSpec
	RequestContext   func(context.Context, core.Session) ([]core.Message, error)
	// PrepareInput captures reference context only at actual input admission.
	// The direct message and its sourced contexts must commit as one batch.
	PrepareInput        func(context.Context, core.Session, core.Message) (core.Message, []core.Message, error)
	CommitPreparedInput func(context.Context, core.Session, string, core.Message, []core.Message) (core.Event, bool, error)
	BeforeModel         func(context.Context, core.Session, string, core.Request) error
	BeforeStep          func(context.Context, core.Session, string, int) error
	BeforeTurn          func(context.Context, core.Session, string) error
	AfterTurn           func(context.Context, core.Session, string) error
	AfterCompaction     func(context.Context, core.Session, string) error
	RenderToolResult    func(context.Context, core.ToolContext, string, core.ToolResult) (core.ToolResult, error)
	Approve             core.Approver
}

type Notice struct {
	SessionID string `json:"session_id"`
	Type      string `json:"type"`
	Data      any    `json:"data"`
}

type Engine struct {
	Store     Store
	Model     core.Model
	Tools     core.Executor
	Options   Options
	Notify    func(Notice)
	mu        sync.Mutex
	running   map[string]context.CancelFunc
	idle      map[string]chan struct{}
	closed    bool
	wg        sync.WaitGroup
	repeat    *RepeatReminder
	liveTools map[string]liveToolExecution
}

func New(store Store, model core.Model, tools core.Executor, opts Options) *Engine {
	if opts.MaxTokens <= 0 {
		opts.MaxTokens = 16384
	}
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = 100
	}
	if opts.ContextTokens <= 0 {
		opts.ContextTokens = 100000
	}
	repeat, _ := NewRepeatReminder(DefaultRepeatConfig())
	return &Engine{Store: store, Model: model, Tools: tools, Options: opts, running: make(map[string]context.CancelFunc), idle: make(map[string]chan struct{}), repeat: repeat}
}

func ID() string { var b [12]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }

func (e *Engine) emit(id, kind string, data any) {
	if e.Notify != nil {
		e.Notify(Notice{SessionID: id, Type: kind, Data: data})
	}
}

func (e *Engine) Cancel(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if cancel, ok := e.running[id]; ok {
		cancel()
		return true
	}
	return false
}

func (e *Engine) Close() {
	e.mu.Lock()
	e.closed = true
	for _, cancel := range e.running {
		cancel()
	}
	e.mu.Unlock()
	e.wg.Wait()
}

func (e *Engine) Busy(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.running[id]
	return ok
}

func (e *Engine) UpdateOptions(fn func(*Options)) { e.mu.Lock(); defer e.mu.Unlock(); fn(&e.Options) }
func (e *Engine) options() Options                { e.mu.Lock(); defer e.mu.Unlock(); return e.Options }

func (e *Engine) Run(ctx context.Context, id string, input core.Message) error {
	return e.run(ctx, id, &input)
}
func (e *Engine) Continue(ctx context.Context, id string) error { return e.run(ctx, id, nil) }

func (e *Engine) run(ctx context.Context, id string, input *core.Message) (err error) {
	if err = ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return errors.New("agent engine is closed")
	}
	if _, ok := e.running[id]; ok {
		e.mu.Unlock()
		return fmt.Errorf("session %s: %w", id, ErrBusy)
	}
	ctx, cancel := context.WithCancel(ctx)
	e.running[id] = cancel
	e.idle[id] = make(chan struct{})
	e.wg.Add(1)
	e.mu.Unlock()
	defer func() {
		cancel()
		e.mu.Lock()
		delete(e.running, id)
		delete(e.liveTools, id)
		close(e.idle[id])
		delete(e.idle, id)
		e.mu.Unlock()
		e.wg.Done()
	}()
	if locker, ok := e.Store.(interface {
		AcquireSession(context.Context, string) (func(), error)
	}); ok {
		release, lockErr := locker.AcquireSession(ctx, id)
		if lockErr != nil {
			return lockErr
		}
		defer release()
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	session, err := e.Store.Get(id)
	if err != nil {
		return err
	}
	if session.Archived {
		return errors.New("cannot run an archived session")
	}
	if input != nil && input.DeliveryID != "" {
		if accepted, err := e.inputAlreadyAccepted(id, input.DeliveryID); err != nil {
			return err
		} else if accepted {
			return nil
		}
	}
	opts := e.options()
	system := opts.System
	if opts.SystemFor != nil {
		system, err = opts.SystemFor(session)
		if err != nil {
			return err
		}
	}
	turn := ID()
	appendEvent := func(kind string, value any) error {
		ev, x := e.Store.Append(id, turn, kind, value)
		if x == nil {
			e.emit(id, "event", ev)
		}
		return x
	}
	if err = appendEvent("turn/started", map[string]any{"id": turn}); err != nil {
		return err
	}
	defer func() {
		if opts.AfterTurn != nil {
			if x := opts.AfterTurn(context.WithoutCancel(ctx), session, turn); x != nil {
				err = errors.Join(err, x)
			}
		}
		status := "completed"
		if err != nil {
			status = "failed"
		}
		if errors.Is(err, context.Canceled) {
			status = "cancelled"
		}
		value := map[string]any{"status": status}
		if err != nil {
			value["error"] = err.Error()
		}
		if x := appendEvent("turn/finished", value); err == nil {
			err = x
		}
		e.emit(id, "turn_finished", value)
	}()
	if opts.BeforeTurn != nil {
		if err = opts.BeforeTurn(ctx, session, turn); err != nil {
			return err
		}
	}
	if input != nil && input.Role != "user" {
		return errors.New("turn input must have user role")
	}
	// A process can die after committing tool_use but before committing its
	// results. Repair that boundary durably before accepting another user turn.
	history, recoverErr := e.Store.Messages(id)
	if recoverErr != nil {
		return recoverErr
	}
	if len(history) > 0 {
		last := history[len(history)-1]
		if last.Role == "assistant" {
			repair := core.Message{Role: "user"}
			for _, b := range last.Content {
				if b.Type == "tool_use" {
					repair.Content = append(repair.Content, core.Block{Type: "tool_result", ToolUseID: b.ID, Content: "Previous process ended before this tool result was committed. Inspect current workspace state before retrying any mutation.", IsError: true})
				}
			}
			if len(repair.Content) > 0 {
				if err = appendEvent("tool/result", repair); err != nil {
					return err
				}
			}
		}
	}
	if input != nil {
		e.repeat.Reset(id)
		prepared, x := e.appendInput(ctx, id, turn, *input)
		if x != nil {
			return x
		}
		input = &prepared
		if err = ctx.Err(); err != nil {
			return err
		}
	}
	if input != nil && (session.Title == "" || session.Title == "New session" || session.Title == "新会话") {
		for _, b := range input.Content {
			if b.Type == "text" {
				r := []rune(strings.Join(strings.Fields(b.Text), " "))
				if len(r) > 40 {
					r = r[:40]
				}
				session.Title = string(r)
				break
			}
		}
		if x := e.Store.Update(session); x != nil {
			return x
		}
	}
	overflowRetries := 0
	for step := 0; step < opts.MaxSteps; step++ {
		if err = ctx.Err(); err != nil {
			return err
		}
		if _, err = e.admitInbox(ctx, id, turn); err != nil {
			return err
		}
		session, err = e.Store.Get(id)
		if err != nil {
			return err
		}
		if opts.BeforeStep != nil {
			if err = opts.BeforeStep(ctx, session, turn, step+1); err != nil {
				return err
			}
			session, err = e.Store.Get(id)
			if err != nil {
				return err
			}
		}
		stepStarted := false
	retryRequest:
		if err = ctx.Err(); err != nil {
			return err
		}
		session, err = e.Store.Get(id)
		if err != nil {
			return err
		}
		messages, x := e.Store.Messages(id)
		if x != nil {
			return x
		}
		model, thinking := opts.Model, opts.Thinking
		if session.Model != "" {
			model = session.Model
		}
		if session.Thinking != "" {
			thinking = session.Thinking
		}
		specs := e.Tools.Specs()
		if opts.ToolsFor != nil {
			specs = opts.ToolsFor(session)
		}
		system = opts.System
		if opts.SystemFor != nil {
			system, err = opts.SystemFor(session)
			if err != nil {
				return err
			}
		}
		provider := session.Provider
		if provider == "" {
			provider = opts.Provider
		}
		var contextMessages []core.Message
		if opts.RequestContext != nil {
			contextMessages, x = opts.RequestContext(ctx, session)
			if x != nil {
				return x
			}
			messages = append(messages, contextMessages...)
		}
		req := core.Request{Provider: provider, Model: model, Thinking: thinking, System: system, Messages: messages, Tools: specs, MaxTokens: opts.MaxTokens, SessionID: id, Purpose: "agent"}
		if req, err = e.relieveContextPressure(ctx, id, turn, req, contextMessages); err != nil {
			return err
		}
		if opts.BeforeModel != nil {
			if err = opts.BeforeModel(ctx, session, turn, req); err != nil {
				return err
			}
		}
		if !stepStarted {
			if err = appendEvent("step/started", map[string]any{"step": step + 1, "model": model}); err != nil {
				return err
			}
			stepStarted = true
		}
		// The request anchor lets the token meter reuse this step's reported
		// usage for an identical later envelope.
		if nodes, _, surfaceErr := e.contextSurface(id); surfaceErr == nil {
			if record, recordErr := contextRequestParts(req, nodes); recordErr == nil {
				if err = appendEvent("context/request", record); err != nil {
					return err
				}
			}
		}
		requestStarted := time.Now()
		var timingMu sync.Mutex
		var firstToken, lastToken *time.Time
		response, x := e.Model.Stream(ctx, req, func(d core.Delta) {
			if d.Text != "" {
				now := time.Now()
				timingMu.Lock()
				if firstToken == nil {
					firstToken = &now
				}
				lastToken = &now
				timingMu.Unlock()
			}
			e.emit(id, "delta", d)
		})
		timingMu.Lock()
		timing := map[string]any{"step": step + 1, "started_at": requestStarted, "completed_at": time.Now(), "first_token_at": firstToken, "last_token_at": lastToken}
		timingMu.Unlock()
		if timingErr := appendEvent("step/timing", timing); timingErr != nil {
			return errors.Join(x, timingErr)
		}
		if x != nil {
			if logErr := appendEvent("assistant/attempt", map[string]any{"response": response, "error": x.Error()}); logErr != nil {
				return errors.Join(x, logErr)
			}
			// A confirmed overflow condenses as much as safely possible and
			// retries the step, at most maxOverflowRetries times per turn.
			if isContextOverflow(x) && overflowRetries < maxOverflowRetries && ctx.Err() == nil {
				overflowRetries++
				recovered, recoverErr := e.recoverOverflow(ctx, id, turn, req)
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if requiredCompactionFailure(recoverErr) {
					return errors.Join(x, recoverErr)
				}
				if recovered {
					if recoverErr != nil {
						e.compactionWarning(id, "context-overflow", "Context recovery made durable progress; retrying the same step.", recoverErr)
					}
					goto retryRequest
				}
				if recoverErr != nil {
					return errors.Join(x, recoverErr)
				}
			}
			return x
		}
		overflowRetries = 0
		if len(response.Content) == 0 {
			return errors.New("model returned an empty response")
		}
		assistant := core.Message{Role: "assistant", Content: response.Content, ReplayModel: model, ReplayProvider: provider, ReplayIdentity: response.ReplayIdentity}
		if err = appendEvent("assistant/message", assistant); err != nil {
			return err
		}
		if err = appendEvent("step/usage", response.Usage); err != nil {
			return err
		}
		calls := []core.Block{}
		for _, b := range response.Content {
			if b.Type == "tool_use" {
				calls = append(calls, b)
			}
		}
		if len(calls) == 0 {
			if response.StopReason == "model_context_window_exceeded" {
				return errors.New("model context window exceeded; use /compact before continuing")
			}
			if response.StopReason == "max_tokens" {
				return errors.New("model output limit reached; use /continue to continue")
			}
			if admitted, x := e.admitInbox(ctx, id, turn); x != nil {
				return x
			} else if admitted {
				continue
			}
			return nil
		}
		results := core.Message{Role: "user", Content: make([]core.Block, 0, len(calls))}
		var inboxMu sync.Mutex
		var inbox []core.Message
		// Serial execution gives file writes and shell calls an unambiguous order.
		// Parallel delegation happens inside the subagent tools.
		for _, call := range calls {
			if err = ctx.Err(); err != nil {
				break
			}
			if err = appendEvent("tool/started", struct {
				core.Block
				InputRaw string `json:"input_raw"`
			}{call, string(call.Input)}); err != nil {
				return err
			}
			e.emit(id, "tool_started", call)
			tc := core.ToolContext{SessionID: id, TurnID: turn, CallID: call.ID, ExecutionID: ID(), CWD: session.CWD, WorkspaceRoot: session.CWD, Permission: session.Permission, ApprovalPolicy: opts.ApprovalPolicy, Approve: opts.Approve}
			tc.Inject = func(message core.Message) error {
				if message.Role != "user" {
					return errors.New("injected message must have user role")
				}
				inboxMu.Lock()
				inbox = append(inbox, message)
				inboxMu.Unlock()
				return nil
			}
			var emitMu sync.Mutex
			var emitErr error
			tc.Emit = func(kind string, data any) error {
				emitMu.Lock()
				defer emitMu.Unlock()
				if emitErr == nil {
					emitErr = appendEvent(kind, data)
				}
				return emitErr
			}
			e.beginToolExecution(tc)
			result, toolErr := e.Tools.Execute(ctx, tc, call.Name, call.Input)
			if notice := e.repeat.Observe(id, call.Name, call.Input); notice != nil {
				message := core.TextMessage("user", notice.Text)
				message.Source = notice.Source
				_ = tc.Inject(message)
			}
			if toolErr != nil {
				result.IsError = true
				result.Content = toolErr.Error()
			}
			if opts.RenderToolResult != nil {
				result, toolErr = opts.RenderToolResult(ctx, tc, call.Name, result)
				if toolErr != nil {
					return toolErr
				}
			}
			emitMu.Lock()
			err = emitErr
			emitMu.Unlock()
			if err != nil {
				return err
			}
			if err = appendEvent("tool/finished", map[string]any{"id": call.ID, "name": call.Name, "result": result}); err != nil {
				return err
			}
			e.endToolExecution(tc)
			e.emit(id, "tool_finished", map[string]any{"name": call.Name, "result": result})
			var content any = result.Content
			if len(result.Blocks) > 0 {
				content = result.Blocks
			}
			results.Content = append(results.Content, core.Block{Type: "tool_result", ToolUseID: call.ID, Content: content, IsError: result.IsError})
		}
		// A cancellation still closes every pending tool_use before a later resume.
		for i := len(results.Content); i < len(calls); i++ {
			results.Content = append(results.Content, core.Block{Type: "tool_result", ToolUseID: calls[i].ID, Content: "Tool execution cancelled before completion.", IsError: true})
		}
		if x = appendEvent("tool/result", results); x != nil {
			return x
		}
		for _, message := range inbox {
			if x = appendEvent("user/message", message); x != nil {
				return x
			}
		}
		if err = ctx.Err(); err != nil {
			return err
		}
	}
	return fmt.Errorf("turn reached the configured limit of %d model steps", opts.MaxSteps)
}

func (e *Engine) Compact(ctx context.Context, id string) error {
	if e.Busy(id) {
		return errors.New("cannot compact a running session")
	}
	if locker, ok := e.Store.(interface {
		AcquireSession(context.Context, string) (func(), error)
	}); ok {
		release, err := locker.AcquireSession(ctx, id)
		if err != nil {
			return err
		}
		defer release()
	}
	messages, err := e.Store.Messages(id)
	if err != nil {
		return err
	}
	return e.compact(ctx, id, ID(), messages)
}

func (e *Engine) compact(ctx context.Context, id, turn string, messages []core.Message) error {
	if len(messages) < 2 {
		return errors.New("not enough history to compact")
	}
	// Compact a prefix ending before a user message, never between a tool call
	// and its result. Keep at least the most recent complete user turn.
	cut := len(messages) - 1
	if store, ok := e.Store.(interface{ CompactionBoundary(string) (int, error) }); ok {
		var err error
		cut, err = store.CompactionBoundary(id)
		if err != nil {
			return err
		}
		if cut >= len(messages) {
			return errors.New("compaction input boundary is outside projected history")
		}
	} else {
		for cut > 0 {
			m := messages[cut]
			if m.Role == "user" && len(m.Content) > 0 && m.Content[0].Type != "tool_result" && !isInputSnapshot(m) {
				break
			}
			cut--
		}
	}
	if cut < 1 {
		return errors.New("no complete history prefix available for compaction")
	}
	return e.compactAt(ctx, id, turn, messages, cut)
}

func Decode(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("invalid tool input: %w", err)
	}
	return nil
}
