package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"dsgo/internal/agent"
	instructions "dsgo/internal/context"
	"dsgo/internal/core"
	"dsgo/internal/schedule"
	"dsgo/internal/tools"
	"dsgo/internal/webtools"
	"dsgo/internal/workflow"
)

type child struct {
	ID, ParentID, Label, Status, Error string
	done                               chan struct{}
	cancel                             context.CancelFunc
	queue                              []string
	ctx                                context.Context
	outcome                            *childOutcome
	seq                                uint64
	official                           bool // started by subagent/subagent_fork: settlement notices reach the parent
	interrupted                        bool // interrupt_agent stops only this child's own work
	torndown                           bool // cascaded cancellation leaves nobody to notify
}
type childOutcome struct {
	done     chan struct{}
	messages []core.Message
	status   string
	err      string
}
type spawnReservation struct {
	parent string
	done   chan struct{}
	cancel context.CancelFunc
}
type parentWake struct {
	target string
	cancel context.CancelFunc
}
type agents struct {
	app             *App
	mu              sync.Mutex
	children        map[string]*child
	closing         bool
	max             int
	active          int // running agents and session creations that have reserved a slot
	starting        map[string]uint64
	wg              sync.WaitGroup
	seq             uint64
	waking          int // settlement notices and wake-up turns still in flight
	reservations    map[uint64]*spawnReservation
	nextReservation uint64
	draining        map[string]int
	wakePaused      map[string]bool
	wakeRuns        map[uint64]*parentWake
	wakeWG          sync.WaitGroup
	nextWake        uint64
}

func newAgents(a *App) *agents {
	return &agents{app: a, children: map[string]*child{}, starting: map[string]uint64{}, max: 8, reservations: map[uint64]*spawnReservation{}, draining: map[string]int{}, wakePaused: map[string]bool{}, wakeRuns: map[uint64]*parentWake{}}
}
func (m *agents) close() {
	m.mu.Lock()
	m.closing = true
	for _, c := range m.children {
		c.cancel()
		c.queue = nil
	}
	for _, reservation := range m.reservations {
		reservation.cancel()
	}
	for _, wake := range m.wakeRuns {
		wake.cancel()
	}
	m.mu.Unlock()
	// All Add calls happen under mu before observing closing. This also waits
	// for an in-flight spawn to release its reservation without publishing work.
	m.wg.Wait()
	m.wakeWG.Wait()
}

func (m *agents) pauseWakes(parent string) {
	m.mu.Lock()
	m.wakePaused[parent] = true
	for _, wake := range m.wakeRuns {
		if m.belongsLocked(wake.target, parent) {
			wake.cancel()
		}
	}
	m.mu.Unlock()
}
func (m *agents) resumeWakes(parent string) { m.mu.Lock(); delete(m.wakePaused, parent); m.mu.Unlock() }

// belongsLocked follows immutable published lineage. The root itself need not
// be a manager child (workflow members are direct Engine sessions).
func (m *agents) belongsLocked(id, root string) bool {
	for id != "" {
		if id == root {
			return true
		}
		c := m.children[id]
		if c == nil {
			return false
		}
		id = c.ParentID
	}
	return false
}
func (m *agents) isDrainingLocked(id string) bool {
	for root, count := range m.draining {
		if count > 0 && m.belongsLocked(id, root) {
			return true
		}
	}
	return false
}

// cancelAndDrain owns precisely this subtree, including starts not yet
// published. Blocking new starts before taking the snapshot closes the
// cancellation/publication race without waiting for unrelated manager work.
func (m *agents) cancelAndDrain(ctx context.Context, parent string) error {
	m.mu.Lock()
	m.draining[parent]++
	m.starting[parent]++
	ids := m.descendantsLocked(parent)
	waits := make([]<-chan struct{}, 0, len(ids)+len(m.reservations))
	for _, id := range ids {
		c := m.children[id]
		m.starting[id]++
		c.torndown = true
		c.queue = nil
		c.cancel()
		waits = append(waits, c.outcome.done)
	}
	for _, reservation := range m.reservations {
		if m.belongsLocked(reservation.parent, parent) {
			reservation.cancel()
			waits = append(waits, reservation.done)
		}
	}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.draining[parent]--
		if m.draining[parent] == 0 {
			delete(m.draining, parent)
		}
		m.mu.Unlock()
	}()
	for _, id := range ids {
		if m.app.registry != nil {
			m.app.registry.InvalidateSession(id)
		}
	}
	for _, done := range waits {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for _, id := range ids {
		if err := m.app.disposeOwner(ctx, id); err != nil {
			return err
		}
	}
	return nil
}
func (m *agents) cancelChildren(parent string) {
	m.mu.Lock()
	ids := m.descendantsLocked(parent)
	// A reserved spawn validates this generation before it publishes a child.
	// Calls that begin after cancellation also check their original tool context.
	m.starting[parent]++
	for _, id := range ids {
		m.starting[id]++
		c := m.children[id]
		c.torndown = true
		c.cancel()
		c.queue = nil
	}
	m.mu.Unlock()
	for _, id := range ids {
		if m.app.registry != nil {
			m.app.registry.InvalidateSession(id)
		}
	}
}

// descendantsLocked returns all owned descendants, including idle children.
// Parentage is immutable after insertion, and all callers hold mu.
func (m *agents) descendantsLocked(parent string) []string {
	var ids []string
	queue := []string{parent}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, c := range m.children {
			if c.ParentID == current {
				ids = append(ids, c.ID)
				queue = append(queue, c.ID)
			}
		}
	}
	return ids
}

// invalidateChildren releases retained code runtimes when a standing policy
// changes. It does not cancel unrelated model work or reset the session logs.
func (m *agents) invalidateChildren(parent string) {
	m.mu.Lock()
	ids := m.descendantsLocked(parent)
	m.mu.Unlock()
	if m.app.registry != nil {
		for _, id := range ids {
			m.app.registry.InvalidateSession(id)
		}
	}
}
func (m *agents) list(parent string) []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []map[string]any{}
	for _, c := range m.children {
		if c.ParentID == parent {
			out = append(out, map[string]any{"id": c.ID, "label": c.Label, "status": c.Status, "error": c.Error})
		}
	}
	return out
}

func (m *agents) spawn(parent, prompt, label, mode string) (string, error) {
	return m.spawnContext(m.app.ctx, parent, prompt, label, mode)
}

func (m *agents) spawnContext(callCtx context.Context, parent, prompt, label, mode string) (string, error) {
	return m.spawnPrepared(callCtx, parent, prompt, label, mode, nil)
}

func (m *agents) spawnPrepared(callCtx context.Context, parent, prompt, label, mode string, beforeDispatch func(string) error) (string, error) {
	if err := callCtx.Err(); err != nil {
		return "", err
	}
	if err := m.app.ctx.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(prompt) == "" {
		return "", errors.New("prompt required")
	}
	return m.spawnChild(callCtx, parent, label, mode, core.TextMessage("user", prompt), false, beforeDispatch)
}

// spawnChild creates and publishes a child; beforeDispatch may still adjust or
// reject the unpublished child Session.
func (m *agents) spawnChild(callCtx context.Context, parent, label, mode string, input core.Message, official bool, beforeDispatch func(string) error) (string, error) {
	if err := callCtx.Err(); err != nil {
		return "", err
	}
	if err := m.app.ctx.Err(); err != nil {
		return "", err
	}
	if mode != "" && mode != "fresh" && mode != "fork" {
		return "", errors.New("subagent mode must be fresh or fork")
	}
	callCtx, reservationCancel := context.WithCancel(callCtx)
	defer reservationCancel()
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return "", errors.New("agent manager closing")
	}
	if m.isDrainingLocked(parent) {
		m.mu.Unlock()
		return "", errors.New("subagent subtree is being disposed")
	}
	if m.active >= m.max {
		m.mu.Unlock()
		return "", fmt.Errorf("maximum of %d active subagents reached", m.max)
	}
	generation := m.starting[parent]
	m.active++
	m.wg.Add(1)
	m.nextReservation++
	reservationID := m.nextReservation
	reservation := &spawnReservation{parent: parent, done: make(chan struct{}), cancel: reservationCancel}
	m.reservations[reservationID] = reservation
	m.mu.Unlock()
	published := false
	defer func() {
		m.mu.Lock()
		if _, exists := m.reservations[reservationID]; exists {
			delete(m.reservations, reservationID)
			close(reservation.done)
		}
		m.mu.Unlock()
		if !published {
			m.mu.Lock()
			m.active--
			m.mu.Unlock()
			m.wg.Done()
		}
	}()
	p, err := m.app.store.Get(parent)
	if err != nil {
		return "", err
	}
	var s core.Session
	if mode == "fork" {
		s, err = m.app.store.ForkCompleted(parent)
	} else {
		s, err = m.app.store.CreateChild(p.CWD, label, parent)
	}
	if err != nil {
		return "", err
	}
	s.ParentID = parent
	s.Delegated = true
	inheritPlan(&s, p)
	s.Provider = p.Provider
	s.Model = p.Model
	s.Thinking = p.Thinking
	s.Preset = p.Preset
	s.ExecutionMode = p.ExecutionMode
	s.Permission = p.Permission
	if err = m.app.store.Update(s); err != nil {
		return "", err
	}
	if beforeDispatch != nil {
		if err = beforeDispatch(s.ID); err != nil {
			return "", errors.Join(err, m.app.store.Delete(s.ID))
		}
	}
	ctx, cancel := context.WithCancel(m.app.ctx)
	done := make(chan struct{})
	outcome := &childOutcome{done: done}
	c := &child{ID: s.ID, ParentID: parent, Label: label, Status: "running", done: done, cancel: cancel, ctx: ctx, outcome: outcome, official: official}
	m.mu.Lock()
	if m.closing || callCtx.Err() != nil || m.app.ctx.Err() != nil || m.starting[parent] != generation {
		m.mu.Unlock()
		cancel()
		if err := callCtx.Err(); err != nil {
			return "", errors.Join(err, m.app.store.Delete(s.ID))
		}
		return "", errors.Join(errors.New("subagent start cancelled before dispatch"), m.app.store.Delete(s.ID))
	}
	m.seq++
	c.seq = m.seq
	m.children[s.ID] = c
	published = true
	delete(m.reservations, reservationID)
	close(reservation.done)
	m.mu.Unlock()
	go m.run(ctx, c, &input, outcome, cancel)
	return s.ID, nil
}

// run drives one activation. A nil input continues the Session so that its
// committed inbox messages are admitted.
func (m *agents) run(ctx context.Context, c *child, input *core.Message, outcome *childOutcome, cancel context.CancelFunc) {
	defer m.wg.Done()
	defer cancel()
	for {
		var err error
		if input == nil {
			err = m.app.engine.Continue(ctx, c.ID)
		} else {
			err = m.app.engine.Run(ctx, c.ID, *input)
		}
		if err == nil {
			err = ctx.Err()
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			cancel()
			m.mu.Lock()
			interrupted := c.interrupted
			m.mu.Unlock()
			if !interrupted {
				m.cancelChildren(c.ID)
			}
		}
		messages, readErr := m.app.store.Messages(c.ID)
		if err == nil && readErr != nil {
			err = readErr
		}
		m.mu.Lock()
		if ctx.Err() == nil && !m.closing {
			if len(c.queue) > 0 {
				next := core.TextMessage("user", c.queue[0])
				input = &next
				c.queue = c.queue[1:]
				m.mu.Unlock()
				continue
			}
			// Deliveries hold mu while committing, so a message that raced the
			// end of this turn is seen here and continues the same activation.
			if pending, _ := m.app.engine.PendingInbox(c.ID); pending {
				input = nil
				m.mu.Unlock()
				continue
			}
		}
		if err != nil {
			c.Status = "failed"
			c.Error = err.Error()
			if errors.Is(err, context.Canceled) {
				c.Status = "cancelled"
			}
		} else {
			c.Status = "completed"
			c.Error = ""
		}
		c.queue = nil
		m.active--
		outcome.messages = messages
		outcome.status = c.Status
		outcome.err = c.Error
		// Publish the final status and close this run's captured channel as one
		// transition. A follow-up can now start a new run without touching it.
		close(outcome.done)
		notify := c.official && !c.torndown && !m.closing && m.app.ctx.Err() == nil
		if notify {
			// Counted before mu is released so settle never sees a gap.
			m.waking++
		}
		parent, status, errText := c.ParentID, c.Status, c.Error
		m.mu.Unlock()
		if notify {
			m.notifySettled(c.ID, parent, status, errText)
		}
		return
	}
}

// resumeLocked restarts an idle child; the caller holds mu and checked capacity.
func (m *agents) resumeLocked(c *child, input *core.Message) {
	delete(m.wakePaused, c.ID)
	childCtx, cancel := context.WithCancel(m.app.ctx)
	c.cancel = cancel
	c.done = make(chan struct{})
	c.outcome = &childOutcome{done: c.done}
	c.ctx = childCtx
	c.Status, c.Error = "running", ""
	c.interrupted, c.torndown = false, false
	m.active++
	m.wg.Add(1)
	go m.run(childCtx, c, input, c.outcome, cancel)
}
func (m *agents) owned(parent, id string) (*child, error) {
	c, ok := m.children[id]
	if !ok || c.ParentID != parent {
		return nil, errors.New("subagent does not belong to this session")
	}
	return c, nil
}

func (a *App) Specs() []core.ToolSpec { return a.registry.Specs() }
func (a *App) Execute(ctx context.Context, tc core.ToolContext, name string, raw json.RawMessage) (core.ToolResult, error) {
	registryName := name
	if a.store != nil && tc.SessionID != "" {
		s, err := a.store.Get(tc.SessionID)
		if err != nil {
			return core.ToolResult{}, err
		}
		allowed := false
		for _, spec := range a.specsFor(s) {
			if spec.Name == name {
				allowed = true
				break
			}
		}
		// Saved dsgo development transcripts may still contain these names;
		// replay them through the same policy while advertising official names.
		if sessionPreset(s) != "minimal" {
			switch name {
			case "read_file", "write_file", "edit_file", "run_shell", "jobs", "spawn_agent", "wait_agent":
				allowed = true
			case "bash":
				registryName = "standard_bash"
			}
		}
		if !allowed {
			return core.ToolResult{}, fmt.Errorf("tool %q is unavailable in preset %q", name, sessionPreset(s))
		}
		tc.WorkspaceRoot = s.CWD
		tc.ExecutionMode = a.sessionMode(s)
		cwd, err := a.ensureWorkingDirectory(ctx, tc.SessionID)
		if err != nil {
			return core.ToolResult{}, err
		}
		tc.CWD = cwd
	}
	if a.planFor(tc.SessionID) {
		switch name {
		case "read", "read_file", "read_image", "list_directory", "search_files", "load_skill", "list_agents", "list_subagent_models", "get_current_time", "schedule_list", "web_search", "web_fetch", "ask_user_question", "exit_plan_mode", "update_goal", "working_directory", "job_output", "job_list":
		case "run_code", "run_shell", "bash":
			var p struct {
				Sandbox            string `json:"sandbox"`
				SandboxPermissions string `json:"sandbox_permissions"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return core.ToolResult{}, err
			}
			if (p.Sandbox != "" && p.Sandbox != "read-only") || p.SandboxPermissions != "" {
				return core.ToolResult{}, errors.New("plan mode permits only read-only sandbox execution")
			}
			tc.Permission = "read-only"
			tc.ApprovalPolicy = "never"
			tc.Approve = nil
		default:
			return core.ToolResult{}, errors.New("plan mode prevents state-changing tools; ask the user to approve with /plan off")
		}
	}
	switch name {
	case "jobs":
		if result, handled, err := a.workflowJobs(ctx, tc, raw); handled {
			return result, err
		}
	case "job_output", "job_list", "job_kill":
		if result, handled, err := a.workflowJobControls(ctx, tc, name, raw); handled {
			return result, err
		}
	}
	return a.registry.Execute(ctx, tc, registryName, raw)
}
func (a *App) systemFor(s core.Session) (string, error) {
	if sessionPreset(s) == "minimal" {
		return "You are a helpful software engineer assistant.", nil
	}
	skills, err := instructions.Skills(s.CWD)
	if err != nil {
		return "", err
	}
	text, err := instructions.System(s.CWD, skills)
	if err != nil {
		return "", err
	}
	if a.planFor(s.ID) {
		text += "\nPlan mode is active. Investigate and propose a plan; the user must explicitly approve via /plan off before state changes.\n"
	}
	text = strings.Replace(text, "You are a coding agent powered by the selected DeepSeek model.", "You are a coding agent powered by the "+s.Model+" model.", 1)
	return text + a.mcpInstructions + a.ptcInstructions(s) + webtools.Guidance() + workflow.Guidance + subagentGuidance, nil
}
func (a *App) loadSkill(name string) (string, error) { return instructions.LoadSkill(a.skills, name) }

func toolSpec(name, desc, schema string) core.ToolSpec {
	return core.ToolSpec{Name: name, Description: desc, InputSchema: json.RawMessage(schema)}
}
func toolValue(v any) core.ToolResult { return core.ToolResult{Content: string(core.JSON(v))} }

func (a *App) registerTools() error {
	defs := []struct {
		s core.ToolSpec
		h tools.Handler
	}{
		{toolSpec("load_skill", "Read the instructions for an available filesystem skill before using it.", `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`), func(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
			var p struct{ Name string }
			if err := agent.Decode(raw, &p); err != nil {
				return core.ToolResult{}, err
			}
			skills, err := instructions.Skills(tc.CWD)
			if err != nil {
				return core.ToolResult{}, err
			}
			text, err := instructions.LoadSkill(skills, p.Name)
			return core.ToolResult{Content: text}, err
		}},
		{toolSpec("ask_user_question", "Ask the user for required information; unavailable interaction is an error, never an implicit answer.", `{"type":"object","properties":{"question":{"type":"string"},"options":{"type":"array","items":{"type":"string"}}},"required":["question"]}`), a.question},
		{toolSpec("update_todo", "Persist the current task list in the session.", `{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{"text":{"type":"string"},"status":{"type":"string","enum":["pending","in_progress","completed"]}},"required":["text","status"]}}},"required":["items"]}`), func(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
			if tc.Emit != nil {
				if err := tc.Emit("todo/updated", raw); err != nil {
					return core.ToolResult{}, err
				}
			}
			return toolValue(map[string]any{"saved": true}), nil
		}},
		{toolSpec("update_goal", "Record the status of an existing user-requested goal. Do not create goals the user did not request.", `{"type":"object","properties":{"objective":{"type":"string"},"status":{"type":"string","enum":["active","complete","paused","blocked"]}},"required":["objective","status"]}`), a.updateGoal},
		{toolSpec("spawn_agent", "Delegate a task to a fresh or forked child agent; children inherit the workspace and permissions.", `{"type":"object","properties":{"prompt":{"type":"string"},"label":{"type":"string"},"mode":{"type":"string","enum":["fresh","fork"]}},"required":["prompt"]}`), func(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
			var p struct{ Prompt, Label, Mode string }
			if err := agent.Decode(raw, &p); err != nil {
				return core.ToolResult{}, err
			}
			if strings.TrimSpace(p.Prompt) == "" {
				return core.ToolResult{}, errors.New("prompt required")
			}
			id, err := a.manager.spawnPrepared(ctx, tc.SessionID, p.Prompt, p.Label, p.Mode, func(id string) error {
				if tc.Emit != nil {
					return tc.Emit("subagent/started", map[string]any{"id": id, "label": p.Label, "mode": p.Mode})
				}
				return nil
			})
			return toolValue(map[string]any{"id": id}), err
		}},
		{toolSpec("wait_agent", "Wait for an owned subagent, returning its settled transcript. Timeout does not cancel the child.", `{"type":"object","properties":{"id":{"type":"string"},"timeout_ms":{"type":"integer"}},"required":["id"]}`), a.waitAgent},
		{toolSpec("get_current_time", "Read the current clock in an IANA timezone.", `{"type":"object","properties":{"timezone":{"type":"string"}}}`), func(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
			var p struct{ Timezone string }
			if err := agent.Decode(raw, &p); err != nil {
				return core.ToolResult{}, err
			}
			if p.Timezone == "" {
				p.Timezone = "UTC"
			}
			loc, err := time.LoadLocation(p.Timezone)
			if err != nil {
				return core.ToolResult{}, err
			}
			return toolValue(map[string]any{"time": time.Now().In(loc).Format(time.RFC3339), "timezone": p.Timezone}), nil
		}},
		{toolSpec("create_reminder", "Persist a reminder for this session. The dsgo daemon must be running for delivery. kind: once/interval/daily/weekly/cron; once uses at RFC3339, interval every duration, daily/weekly clock HH:MM, weekly weekday 0..6, cron five fields. timezone uses IANA names.", `{"type":"object","properties":{"prompt":{"type":"string"},"kind":{"type":"string"},"at":{"type":"string"},"every":{"type":"string"},"clock":{"type":"string"},"weekday":{"type":"integer"},"cron":{"type":"string"},"timezone":{"type":"string"}},"required":["prompt","kind"]}`), func(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
			var t schedule.Task
			if err := agent.Decode(raw, &t); err != nil {
				return core.ToolResult{}, err
			}
			t.SessionID = tc.SessionID
			t, err := a.schedules.Create(t)
			if err == nil && tc.Emit != nil {
				tc.Emit("schedule/created", t)
			}
			return toolValue(t), err
		}},
		{toolSpec("list_reminders", "List this session's durable reminders.", `{"type":"object","properties":{}}`), func(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
			all, err := a.schedules.List()
			if err != nil {
				return core.ToolResult{}, err
			}
			out := []schedule.Task{}
			for _, t := range all {
				if t.SessionID == tc.SessionID {
					out = append(out, t)
				}
			}
			return toolValue(out), nil
		}},
		{toolSpec("cancel_reminder", "Cancel an owned reminder.", `{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`), func(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
			var p struct{ ID string }
			if err := agent.Decode(raw, &p); err != nil {
				return core.ToolResult{}, err
			}
			tasks, err := a.schedules.List()
			if err != nil {
				return core.ToolResult{}, err
			}
			for _, t := range tasks {
				if t.ID == p.ID && t.SessionID == tc.SessionID {
					return toolValue(map[string]any{"cancelled": true}), a.schedules.Cancel(p.ID)
				}
			}
			return core.ToolResult{}, errors.New("reminder does not belong to this session")
		}},
	}
	defs = append(defs, struct {
		s core.ToolSpec
		h tools.Handler
	}{toolSpec("exit_plan_mode", "Submit a complete plan for explicit approval. If interaction is unavailable, stay in plan mode.", `{"type":"object","properties":{"plan":{"type":"string"}},"required":["plan"]}`), func(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
		if tc.SessionID != a.current.Load() {
			return core.ToolResult{}, errors.New("a child cannot change the parent's plan mode")
		}
		var p struct{ Plan string }
		if err := agent.Decode(raw, &p); err != nil {
			return core.ToolResult{}, err
		}
		if !strings.HasPrefix(strings.TrimSpace(p.Plan), "# ") {
			return core.ToolResult{}, errors.New("plan must start with a Markdown title")
		}
		if !a.planFor(tc.SessionID) {
			return core.ToolResult{}, errors.New("session is not in plan mode")
		}
		q := core.Approval{ID: agent.ID(), SessionID: tc.SessionID, Tool: "exit_plan_mode", Description: p.Plan, Input: raw}
		if tc.Emit != nil {
			if err := tc.Emit("approval/requested", q); err != nil {
				return core.ToolResult{}, err
			}
		}
		ok, err := a.approve(ctx, q)
		if tc.Emit != nil {
			if logErr := tc.Emit("approval/resolved", map[string]any{"id": q.ID, "allowed": ok && err == nil && ctx.Err() == nil}); logErr != nil {
				return core.ToolResult{}, logErr
			}
		}
		if err != nil {
			return core.ToolResult{}, err
		}
		if !ok {
			return core.ToolResult{}, errors.New("plan was not approved; remain in plan mode")
		}
		if err := ctx.Err(); err != nil {
			return core.ToolResult{}, err
		}
		if tc.Emit != nil {
			if err := tc.Emit("plan/approved", raw); err != nil {
				return core.ToolResult{}, err
			}
		}
		if err := a.setPlan(tc.SessionID, false); err != nil {
			return core.ToolResult{}, err
		}
		return toolValue(map[string]any{"approved": true}), nil
	}})
	for _, d := range defs {
		if d.s.Name == "create_reminder" || d.s.Name == "list_reminders" || d.s.Name == "cancel_reminder" || d.s.Name == "update_todo" {
			continue
		}
		if err := a.registry.Register(d.s, d.h); err != nil {
			return err
		}
	}
	if err := a.registerSubagentTools(); err != nil {
		return err
	}
	a.registry.SetJobNotifier(a.deliverJobNotice)
	if err := a.registerTodoTool(); err != nil {
		return err
	}
	if err := a.registerWorkingDirectoryTool(); err != nil {
		return err
	}
	if err := a.registerArtifactTools(); err != nil {
		return err
	}
	if err := a.registerImageTool(); err != nil {
		return err
	}
	if err := a.registerWorkflowTools(); err != nil {
		return err
	}
	if err := a.registerScheduleTools(); err != nil {
		return err
	}
	key, token, err := credentials(a.cfg)
	if err != nil {
		return err
	}
	if !officialAPI(a.cfg.BaseURL) {
		// Web search is an independent official route. A custom model endpoint's
		// credential must not be forwarded to the official search service.
		key = os.Getenv("DEEPSEEK_API_KEY")
	}
	web := webtools.New(webtools.Options{APIKey: key, Token: token, OriginalQuestion: func(id string) string {
		messages, err := a.store.Messages(id)
		if err != nil {
			return ""
		}
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == "user" {
				for _, b := range messages[i].Content {
					if b.Type == "text" {
						return b.Text
					}
				}
			}
		}
		return ""
	}})
	for _, spec := range web.Specs() {
		name := spec.Name
		if err := a.registry.Register(spec, func(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
			return web.Execute(ctx, tc, name, raw)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) question(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	if a.unattended || a.flags.json {
		return core.ToolResult{}, errors.New("user question requires interactive input")
	}
	var p struct {
		Question string
		Options  []string
	}
	if err := agent.Decode(raw, &p); err != nil {
		return core.ToolResult{}, err
	}
	a.inputMu.Lock()
	defer a.inputMu.Unlock()
	a.line("\n%s\n", clean(p.Question))
	for i, s := range p.Options {
		a.line("  %d. %s\n", i+1, clean(s))
	}
	a.line("Answer: ")
	answer, err := a.readLine(ctx)
	if err != nil {
		return core.ToolResult{}, err
	}
	if tc.Emit != nil {
		if err := tc.Emit("user/answer", map[string]any{"question": p.Question, "answer": answer}); err != nil {
			return core.ToolResult{}, err
		}
	}
	return toolValue(map[string]any{"answer": answer}), nil
}
func (a *App) agentMessage(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	var p struct{ ID, Message string }
	if err := agent.Decode(raw, &p); err != nil {
		return core.ToolResult{}, err
	}
	m := a.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return core.ToolResult{}, err
	}
	if m.closing {
		return core.ToolResult{}, errors.New("agent manager closing")
	}
	c, err := m.owned(tc.SessionID, p.ID)
	if err != nil {
		return core.ToolResult{}, err
	}
	if strings.TrimSpace(p.Message) == "" {
		return core.ToolResult{}, errors.New("message is empty")
	}
	if c.Status == "running" {
		if err := c.ctx.Err(); err != nil {
			return core.ToolResult{}, errors.New("subagent is cancelling; wait before sending a follow-up")
		}
		if len(c.queue) >= 128 {
			return core.ToolResult{}, errors.New("subagent follow-up queue is full")
		}
		c.queue = append(c.queue, p.Message)
	} else {
		if m.active >= m.max {
			return core.ToolResult{}, fmt.Errorf("maximum of %d active subagents reached", m.max)
		}
		input := core.TextMessage("user", p.Message)
		m.resumeLocked(c, &input)
	}
	return toolValue(map[string]any{"queued": true}), nil
}
func (a *App) waitAgent(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	var p struct {
		ID        string
		TimeoutMS int `json:"timeout_ms"`
	}
	if err := agent.Decode(raw, &p); err != nil {
		return core.ToolResult{}, err
	}
	m := a.manager
	m.mu.Lock()
	c, err := m.owned(tc.SessionID, p.ID)
	if err != nil {
		m.mu.Unlock()
		return core.ToolResult{}, err
	}
	outcome := c.outcome
	m.mu.Unlock()
	if p.TimeoutMS <= 0 {
		p.TimeoutMS = 30000
	}
	if p.TimeoutMS > 600000 {
		p.TimeoutMS = 600000
	}
	select {
	case <-outcome.done:
		return toolValue(map[string]any{"id": p.ID, "status": outcome.status, "error": outcome.err, "messages": outcome.messages}), nil
	case <-ctx.Done():
		return core.ToolResult{}, ctx.Err()
	case <-time.After(time.Duration(p.TimeoutMS) * time.Millisecond):
		return toolValue(map[string]any{"id": p.ID, "running": true}), nil
	}
}
func signalContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
}
