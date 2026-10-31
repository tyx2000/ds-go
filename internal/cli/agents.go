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
	"dsgo/internal/tools"
	"dsgo/internal/webtools"
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
	settling        map[string]int // completion notices not yet accepted by their parent
}

func newAgents(a *App) *agents {
	max := a.cfg.SubagentMaxActive
	if max <= 0 {
		max = 8
	}
	return &agents{app: a, children: map[string]*child{}, starting: map[string]uint64{}, max: max, reservations: map[uint64]*spawnReservation{}, draining: map[string]int{}, wakePaused: map[string]bool{}, wakeRuns: map[uint64]*parentWake{}, settling: map[string]int{}}
}

// Residency, rather than historical Session ancestry, identifies an activation
// pool. A top-level resumed/forked conversation starts its own pool even when
// its durable header still records an older delegated origin.
func (m *agents) activationRootLocked(id string) string {
	for {
		c := m.children[id]
		if c == nil {
			return id
		}
		id = c.ParentID
	}
}

func (m *agents) activeInTreeLocked(parent string) int {
	root := m.activationRootLocked(parent)
	count := 0
	for id, c := range m.children {
		if c.Status == "running" && m.activationRootLocked(id) == root {
			count++
		}
	}
	for _, reservation := range m.reservations {
		if m.activationRootLocked(reservation.parent) == root {
			count++
		}
	}
	return count
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
	if m.activeInTreeLocked(parent) >= m.max {
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
	s.CreationLabel = label
	inheritPlan(&s, p)
	s.Provider = p.Provider
	s.Model = p.Model
	s.Thinking = p.Thinking
	s.Permission = p.Permission
	if s.Permission == "" {
		s.Permission = m.app.cfg.Permission
	}
	if err = m.app.store.Update(s); err != nil {
		return "", err
	}
	if beforeDispatch != nil {
		if err = beforeDispatch(s.ID); err != nil {
			return "", errors.Join(err, m.app.store.Delete(s.ID))
		}
	}
	descriptor, err := m.describeChild(s.ID, label, mode)
	if err != nil {
		return "", errors.Join(err, m.app.store.Delete(s.ID))
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
	if err := m.establishCatalogChild(parent, s, descriptor); err != nil {
		m.mu.Unlock()
		cancel()
		return "", errors.Join(err, m.app.store.Delete(s.ID))
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
			m.settling[c.ID]++
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
func (a *App) Execute(ctx context.Context, tc core.ToolContext, name string, raw json.RawMessage) (result core.ToolResult, err error) {
	if a.store != nil {
		tc.HarnessHome = a.store.Root()
	}
	a.beginJobOutputBound(tc, name, raw)
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
		if name == "bash" {
			registryName = "standard_bash"
		}
		if !allowed {
			return core.ToolResult{}, fmt.Errorf("tool %q is unavailable", name)
		}
		tc.WorkspaceRoot = s.CWD
		if s.Delegated {
			// A delegated child cannot request an approval which widens the
			// scope captured at its creation or cold restoration.
			tc.Permission = s.Permission
			tc.ApprovalPolicy = "never"
			tc.Approve = nil
		}
		cwd, err := a.ensureWorkingDirectory(ctx, tc.SessionID)
		if err != nil {
			return core.ToolResult{}, err
		}
		tc.CWD = cwd
	}

	return a.registry.Execute(ctx, tc, registryName, raw)
}
func (a *App) systemFor(s core.Session) (string, error) {
	skills, err := instructions.Skills(s.CWD)
	if err != nil {
		return "", err
	}
	return a.presetSystemPrompt(s, skills), nil
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
		{userQuestionSpec(), a.question},
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
	}
	defs = append(defs, struct {
		s core.ToolSpec
		h tools.Handler
	}{toolSpec("exit_plan_mode", "Use only in plan mode. Present your complete markdown plan for review; approval leaves plan mode from the next step, while feedback keeps planning.", `{"type":"object","additionalProperties":false,"properties":{"plan":{"type":"string"}},"required":["plan"]}`), a.exitPlanMode})
	for _, d := range defs {
		if err := a.registry.Register(d.s, d.h); err != nil {
			return err
		}
	}
	if err := a.registerSubagentTools(); err != nil {
		return err
	}
	if err := a.registerGoalTools(); err != nil {
		return err
	}
	a.registry.SetJobNotifier(a.deliverJobNotice)
	if err := a.registerTodoTool(); err != nil {
		return err
	}
	if err := a.registerWorkingDirectoryTool(); err != nil {
		return err
	}
	if err := a.registerImageTool(); err != nil {
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
		if m.activeInTreeLocked(c.ID) >= m.max {
			return core.ToolResult{}, fmt.Errorf("maximum of %d active subagents reached", m.max)
		}
		input := core.TextMessage("user", p.Message)
		m.resumeLocked(c, &input)
	}
	return toolValue(map[string]any{"queued": true}), nil
}
func signalContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
}

// disposeOwner quiesces resources owned by one completed session without
// touching unrelated sessions or the application.
func (a *App) disposeOwner(ctx context.Context, id string) error {
	if a.manager != nil {
		a.manager.pauseWakes(id)
	}
	if a.registry != nil {
		if err := a.registry.CancelSessionJobs(ctx, id); err != nil {
			return err
		}
		a.registry.InvalidateSession(id)
	}
	return nil
}
