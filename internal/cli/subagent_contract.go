package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"dsgo/internal/core"
	"dsgo/internal/model"
)

const subagentGuidance = "\nStart independent delegations with `subagent` or `subagent_fork` together in one assistant message and continue useful work while they run.\n"

// presentation carries a tool's official model text; programs keep the value.
type presentation struct{ text string }

func presented(value any, text string) core.ToolResult {
	return core.ToolResult{Content: string(core.JSON(value)), Meta: presentation{text}}
}

// These adapters follow the pinned official tool-subagent and tool-subagent-control
// contracts. Provider means an existing configured LLM route, never a new backend.
type subagentRoute struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}
type subagentPolicy struct {
	AllowedModels []subagentRoute `json:"allowedModels"`
}
type subagentSelectionSetting struct {
	Enabled       bool            `json:"enabled"`
	AllowedModels []subagentRoute `json:"allowedModels"`
}
type subagentStart struct {
	Prompt, Description, Mode, CWD   string
	Provider, Model, ReasoningEffort *string
	MaxDepth                         int
}
type subagentCatalogEntry struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Label  string `json:"label,omitempty"`
	Status string `json:"status,omitempty"`
	Reason string `json:"reason,omitempty"`
	Parent string `json:"parent,omitempty"`
	Depth  int    `json:"depth,omitempty"`
}

func (a *App) subagentPolicy(id string) (*subagentPolicy, error) {
	seen := map[string]bool{}
	for id != "" {
		if seen[id] {
			return nil, errors.New("corrupt subagent lineage")
		}
		seen[id] = true
		events, err := a.store.Events(id)
		if err != nil {
			return nil, err
		}
		for i := len(events) - 1; i >= 0; i-- {
			if events[i].Type == "subagent/model-selection" {
				var p subagentPolicy
				if err = json.Unmarshal(events[i].Data, &p); err != nil || len(p.AllowedModels) == 0 {
					return nil, errors.New("invalid stored subagent model policy")
				}
				return &p, nil
			}
		}
		session, err := a.store.Get(id)
		if err != nil {
			return nil, err
		}
		if !session.Delegated {
			return nil, nil
		}
		id = session.ParentID
	}
	return nil, nil
}

// sampleSubagentPolicy is called only while a new top-level Session is unpublished.
// A restored session without a recorded opt-in stays disabled after settings edits.
func (a *App) sampleSubagentPolicy(id string) error {
	session, err := a.store.Get(id)
	if err != nil {
		return err
	}
	if session.Delegated {
		return nil
	}
	events, err := a.store.Events(id)
	if err != nil {
		return err
	}
	if len(events) > 0 || len(a.cfg.SubagentModelSelection) == 0 {
		return nil
	}
	var setting subagentSelectionSetting
	if err = decodeOfficialObject(a.cfg.SubagentModelSelection, &setting); err != nil {
		return err
	}
	if !setting.Enabled {
		return nil
	}
	if len(setting.AllowedModels) == 0 {
		return errors.New("enabled subagent model selection requires at least one exact provider/model route")
	}
	for _, route := range setting.AllowedModels {
		if route.Provider == "" || route.Provider != strings.TrimSpace(route.Provider) || route.Model == "" || route.Model != strings.TrimSpace(route.Model) {
			return errors.New("subagent allowedModels requires non-empty normalized provider and model")
		}
		if route.Provider != "deepseek-official" {
			if _, ok := a.cfg.ProviderProfiles[route.Provider]; !ok {
				return fmt.Errorf("subagent provider %q is not configured", route.Provider)
			}
		}
	}
	_, err = a.store.Append(id, "", "subagent/model-selection", subagentPolicy{AllowedModels: setting.AllowedModels})
	return err
}
func (a *App) inheritSubagentPolicy(parent, child string) error {
	policy, err := a.subagentPolicy(parent)
	if err != nil || policy == nil {
		return err
	}
	_, err = a.store.Append(child, "", "subagent/model-selection", policy)
	return err
}
func (a *App) subagentDepth(id string) (int, error) {
	depth := 0
	seen := map[string]bool{}
	for id != "" {
		if seen[id] {
			return 0, errors.New("corrupt subagent lineage")
		}
		seen[id] = true
		s, err := a.store.Get(id)
		if err != nil {
			return 0, err
		}
		if !s.Delegated {
			return depth, nil
		}
		depth++
		id = s.ParentID
	}
	return depth, nil
}
func (a *App) subagentMaxDepth() int {
	if a.cfg.SubagentMaxDepth != nil {
		return *a.cfg.SubagentMaxDepth
	}
	return 1
}
func (a *App) subagentSpecFor(session core.Session, fork bool) core.ToolSpec {
	enabled := false
	if !fork {
		policy, err := a.subagentPolicy(session.ID)
		enabled = err == nil && policy != nil
	}
	return delegationSpec(fork, enabled)
}
func delegationSpec(fork, selection bool) core.ToolSpec {
	name, desc, prompt := "subagent", "Delegate a self-contained task in a separate fresh context. The child does not share this conversation. Start returns its id immediately; completion and messages are delivered independently.", "Complete self-contained task, including all needed context."
	if fork {
		name, desc, prompt = "subagent_fork", "Delegate work to a child seeded with completed turns of this conversation. The current in-flight turn is excluded. Start returns its id immediately; completion and messages are delivered independently.", "The task; the child already sees completed turns, so state what is new."
	}
	properties := map[string]any{"description": map[string]any{"type": "string", "description": "Short 3-5 word task description for display."}, "prompt": map[string]any{"type": "string", "description": prompt}, "cwd": map[string]any{"type": "string", "description": "Initial child working directory; relative to current directory, omitted inherits it. Later changes are independent."}}
	if selection && !fork {
		desc += " Optional child LLM selection uses the Session's approved exact routes; inspect list_subagent_models first. Supply provider and model together. A changed route without effort uses its model default."
		properties["provider"] = map[string]any{"type": "string"}
		properties["model"] = map[string]any{"type": "string"}
		properties["reasoning_effort"] = map[string]any{"type": "string"}
	}
	return core.ToolSpec{Name: name, Description: desc, InputSchema: core.JSON(map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "required": []string{"description", "prompt"}})}
}
func (a *App) registerSubagentTools() error {
	for _, fork := range []bool{false, true} {
		isFork := fork
		if err := a.registry.Register(delegationSpec(isFork, !isFork), func(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
			return a.delegateOfficial(ctx, tc, raw, isFork)
		}); err != nil {
			return err
		}
	}
	handlers := map[string]func(context.Context, core.ToolContext, json.RawMessage) (core.ToolResult, error){"send_message": a.sendMessageOfficial, "interrupt_agent": a.interruptAgentOfficial, "list_agents": a.listAgentsOfficial}
	for _, spec := range officialAgentControlSpecs() {
		if err := a.registry.Register(spec, handlers[spec.Name]); err != nil {
			return err
		}
	}
	return a.registry.Register(toolSpec("list_subagent_models", "Inspect approved child LLM routes: omit arguments for providers, provider for advertised models, provider and model for exact model reasoning efforts. This does not change the current agent.", `{"type":"object","additionalProperties":false,"properties":{"provider":{"type":"string"},"model":{"type":"string"}}}`), a.listSubagentModels)
}
func (a *App) delegateOfficial(ctx context.Context, tc core.ToolContext, raw json.RawMessage, fork bool) (core.ToolResult, error) {
	var p struct {
		Description *string `json:"description"`
		Prompt      *string `json:"prompt"`
		CWD         *string `json:"cwd"`
		Provider    *string `json:"provider"`
		Model       *string `json:"model"`
		Effort      *string `json:"reasoning_effort"`
	}
	if err := decodeOfficialObject(raw, &p); err != nil {
		return core.ToolResult{}, err
	}
	if p.Description == nil || p.Prompt == nil {
		return core.ToolResult{}, errors.New("description and prompt are required strings")
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	for _, name := range []string{"cwd", "provider", "model", "reasoning_effort"} {
		if v, ok := fields[name]; ok && string(v) == "null" {
			return core.ToolResult{}, fmt.Errorf("%s must be a string", name)
		}
	}
	if _, err := a.store.Get(tc.SessionID); err != nil {
		return core.ToolResult{}, errors.New("subagent requires a calling agent")
	}
	maxDepth := a.subagentMaxDepth()
	depth, err := a.subagentDepth(tc.SessionID)
	if err != nil {
		return core.ToolResult{}, err
	}
	if maxDepth < 0 || depth >= maxDepth {
		return core.ToolResult{}, fmt.Errorf("delegation depth %d reached maximum %d", depth, maxDepth)
	}
	if (p.Provider == nil) != (p.Model == nil) {
		return core.ToolResult{}, errors.New("provider and model must be supplied together")
	}
	if fork && (p.Provider != nil || p.Model != nil || p.Effort != nil) {
		return core.ToolResult{}, errors.New("subagent_fork does not expose child LLM selection")
	}
	if p.Provider != nil || p.Effort != nil {
		policy, err := a.subagentPolicy(tc.SessionID)
		if err != nil {
			return core.ToolResult{}, err
		}
		if policy == nil {
			return core.ToolResult{}, errors.New("child LLM selection is disabled for this Session")
		}
		parent, err := a.store.Get(tc.SessionID)
		if err != nil {
			return core.ToolResult{}, err
		}
		provider, modelID := parent.Provider, parent.Model
		if provider == "" {
			provider = a.cfg.SelectedProvider
		}
		if modelID == "" {
			modelID = a.cfg.Model
		}
		if p.Provider != nil {
			provider, modelID = *p.Provider, *p.Model
		}
		if provider == "" || modelID == "" {
			return core.ToolResult{}, errors.New("provider and model must be non-empty")
		}
		allowed := false
		for _, route := range policy.AllowedModels {
			if route.Provider == provider && route.Model == modelID {
				allowed = true
				break
			}
		}
		if !allowed {
			return core.ToolResult{}, fmt.Errorf("child LLM route %q is not allowed for this Session", provider+"/"+modelID)
		}
		if p.Effort != nil && *p.Effort == "" {
			return core.ToolResult{}, errors.New("reasoning_effort must be non-empty")
		}
	}
	req := subagentStart{Prompt: *p.Prompt, Description: *p.Description, Mode: "fresh", Provider: p.Provider, Model: p.Model, ReasoningEffort: p.Effort, MaxDepth: maxDepth}
	if p.CWD != nil {
		req.CWD = *p.CWD
	}
	if fork {
		req.Mode = "fork"
	}
	id, err := a.manager.spawnOfficial(ctx, tc, req, func(id string) error {
		if tc.Emit != nil {
			return tc.Emit("subagent/started", map[string]any{"id": id, "label": req.Description, "mode": req.Mode})
		}
		return nil
	})
	if err != nil {
		return core.ToolResult{}, err
	}
	return presented(map[string]any{"kind": "activation", "subagentId": id}, "started subagent "+id), nil
}
func officialAgentControlSpecs() []core.ToolSpec {
	return []core.ToolSpec{toolSpec("send_message", "Send to a direct continuable child, or to your direct parent while resident. Working agents receive it at the next step; inactive children resume. Returns delivery confirmation, not an answer.", `{"type":"object","additionalProperties":false,"properties":{"agent_id":{"type":"string"},"message":{"type":"string"}},"required":["agent_id","message"]}`), toolSpec("interrupt_agent", "Ask a descendant to stop its current work and return immediately. Its own descendants keep running. Direct local child conversations can later continue with send_message.", `{"type":"object","additionalProperties":false,"properties":{"agent_id":{"type":"string"}},"required":["agent_id"]}`), toolSpec("list_agents", "List continuable subagents. running means working; inactive means not working. Completion is delivered without polling. children accepts follow-ups; descendants also reports parent/depth and deeper descendants accept interruption only.", `{"type":"object","additionalProperties":false,"properties":{"scope":{"type":"string","enum":["children","descendants"]}}}`)}
}
func (a *App) sendMessageOfficial(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	var p struct {
		AgentID *string `json:"agent_id"`
		Message *string `json:"message"`
	}
	if err := decodeOfficialObject(raw, &p); err != nil {
		return core.ToolResult{}, err
	}
	if p.AgentID == nil || p.Message == nil {
		return core.ToolResult{}, errors.New("agent_id and message are required strings")
	}
	id, err := a.manager.sendOfficial(ctx, tc.SessionID, *p.AgentID, *p.Message)
	if err != nil {
		return core.ToolResult{}, err
	}
	return presented(map[string]any{"messageId": id}, "message delivered to agent "+*p.AgentID), nil
}
func (a *App) interruptAgentOfficial(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return core.ToolResult{}, err
	}
	var p struct {
		AgentID *string `json:"agent_id"`
	}
	if err := decodeOfficialObject(raw, &p); err != nil {
		return core.ToolResult{}, err
	}
	if p.AgentID == nil {
		return core.ToolResult{}, errors.New("agent_id is required")
	}
	if err := a.manager.interruptOfficial(tc.SessionID, *p.AgentID); err != nil {
		return core.ToolResult{}, err
	}
	return presented(map[string]any{"accepted": true}, "interrupt requested for agent "+*p.AgentID), nil
}
func (a *App) listAgentsOfficial(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	var p struct {
		Scope *string `json:"scope"`
	}
	if err := decodeOfficialObject(raw, &p); err != nil {
		return core.ToolResult{}, err
	}
	scope := "children"
	if p.Scope != nil {
		scope = *p.Scope
	}
	if scope != "children" && scope != "descendants" {
		return core.ToolResult{}, errors.New("scope must be children or descendants")
	}
	rows, err := a.manager.catalogOfficial(ctx, tc.SessionID, scope == "descendants")
	if err != nil {
		return core.ToolResult{}, err
	}
	lines := make([]string, len(rows))
	for i, row := range rows {
		at := ""
		if scope == "descendants" {
			at = fmt.Sprintf(" parent=%s depth=%d", row.Parent, row.Depth)
		}
		lines[i] = fmt.Sprintf("%s [%s]%s — %s", row.ID, row.Status, at, row.Label)
	}
	text := strings.Join(lines, "\n")
	if len(rows) == 0 {
		text = "(no subagents)"
	}
	return presented(rows, text), nil
}
func (a *App) listSubagentModels(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	var p struct {
		Provider *string `json:"provider"`
		Model    *string `json:"model"`
	}
	if err := decodeOfficialObject(raw, &p); err != nil {
		return core.ToolResult{}, err
	}
	policy, err := a.subagentPolicy(tc.SessionID)
	if err != nil {
		return core.ToolResult{}, err
	}
	if policy == nil {
		return core.ToolResult{}, errors.New("child LLM selection is disabled for this Session")
	}
	if p.Model != nil && p.Provider == nil {
		return core.ToolResult{}, errors.New("model requires provider")
	}
	registered := func(id string) bool {
		if id == "deepseek-official" {
			return true
		}
		_, ok := a.cfg.ProviderProfiles[id]
		return ok
	}
	if p.Provider == nil {
		ids := map[string]bool{}
		for _, route := range policy.AllowedModels {
			if registered(route.Provider) {
				ids[route.Provider] = true
			}
		}
		list := []string{}
		for id := range ids {
			list = append(list, id+" — "+id)
		}
		sort.Strings(list)
		if len(list) == 0 {
			return core.ToolResult{Content: "(no LLM providers)"}, nil
		}
		return core.ToolResult{Content: strings.Join(list, "\n")}, nil
	}
	if *p.Provider == "" {
		return core.ToolResult{}, errors.New("provider must be non-empty")
	}
	allowed := map[string]bool{}
	for _, route := range policy.AllowedModels {
		if route.Provider == *p.Provider {
			allowed[route.Model] = true
		}
	}
	if len(allowed) == 0 {
		return core.ToolResult{}, fmt.Errorf("LLM provider %q is not allowed for this Session", *p.Provider)
	}
	if !registered(*p.Provider) {
		return core.ToolResult{}, fmt.Errorf("LLM provider %q is not registered", *p.Provider)
	}
	if p.Model != nil {
		if *p.Model == "" || !allowed[*p.Model] {
			return core.ToolResult{}, errors.New("exact child LLM route is not allowed for this Session")
		}
		return core.ToolResult{Content: *p.Provider + "/" + *p.Model + " — " + *p.Model + "\nReasoning efforts:\n(no advertised reasoning efforts)"}, nil
	}
	var models []model.ModelInfo
	if *p.Provider == "deepseek-official" {
		models = model.BuiltinModels()
	} else {
		var options model.ProviderOptions
		if err = json.Unmarshal(a.cfg.ProviderProfiles[*p.Provider], &options); err != nil {
			return core.ToolResult{}, err
		}
		client, err := model.NewProvider(options)
		if err != nil {
			return core.ToolResult{}, err
		}
		models, err = client.ListModels(ctx)
		if err != nil {
			return core.ToolResult{}, err
		}
	}
	lines := []string{}
	for _, info := range models {
		if allowed[info.ID] {
			name := info.Name
			if name == "" {
				name = info.ID
			}
			lines = append(lines, *p.Provider+"/"+info.ID+" — "+name)
		}
	}
	if len(lines) == 0 {
		return core.ToolResult{Content: "(no advertised models for " + *p.Provider + ")"}, nil
	}
	return core.ToolResult{Content: strings.Join(lines, "\n")}, nil
}

var errManagerClosing = errors.New("agent manager closing")

func continuableReturnGuidance(parent string) string {
	id := officialJSONString(parent)
	return "Your parent agent id is " + id + ". Before you finish, send your result to that agent with " +
		"send_message({ agent_id: " + id + `, message: "<self-contained result>" }). The parent shares ` +
		"your workspace but does not automatically receive your transcript, tool output, or reasoning. Send " +
		"earlier messages as well when a finding changes what the parent should do next; sending a message " +
		"does not end your turn."
}

// spawnOfficial starts a continuable local child whose settlement notice
// reaches the caller. Route, effort and directory apply before dispatch.
func (m *agents) spawnOfficial(ctx context.Context, tc core.ToolContext, req subagentStart, published func(string) error) (string, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return "", errors.New("prompt must be a non-empty string")
	}
	current := tc.CWD
	if current == "" {
		var err error
		if current, err = m.app.ensureWorkingDirectory(ctx, tc.SessionID); err != nil {
			return "", err
		}
	}
	cwd := current
	if req.CWD != "" {
		var err error
		if cwd, err = resolveWorkingDirectory(req.CWD, current); err != nil {
			return "", err
		}
	}
	input := core.Message{Role: "user", Content: []core.Block{{Type: "text", Text: req.Prompt}, {Type: "text", Text: continuableReturnGuidance(tc.SessionID)}}}
	return m.spawnChild(ctx, tc.SessionID, req.Description, req.Mode, input, true, func(id string) error {
		s, err := m.app.store.Get(id)
		if err != nil {
			return err
		}
		if req.Provider != nil {
			// A changed route without an effort resolves its model default.
			s.Provider, s.Model, s.Thinking = *req.Provider, *req.Model, ""
		}
		if req.ReasoningEffort != nil {
			s.Thinking = *req.ReasoningEffort
		}
		if err = m.app.store.Update(s); err != nil {
			return err
		}
		if err = m.app.inheritSubagentPolicy(tc.SessionID, id); err != nil {
			return err
		}
		active, err := m.app.activeWorkingDirectory(id)
		if err != nil {
			return err
		}
		if active != cwd {
			if _, err = m.app.store.Append(id, "", "working-directory/change", map[string]any{"cwd": cwd}); err != nil {
				return err
			}
		}
		return published(id)
	})
}

// sendOfficial delivers between adjacent agents: a direct child, or the
// sender's direct parent while the sender is a resident child.
func (m *agents) sendOfficial(ctx context.Context, sender, target, text string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(text) == "" {
		return "", errors.New("message must be a non-empty string")
	}
	m.mu.Lock()
	t, ok := m.children[target]
	toChild := ok && t.ParentID == sender
	s, ok := m.children[sender]
	toParent := ok && s.ParentID == target && s.Status == "running"
	m.mu.Unlock()
	if !toChild && !toParent {
		return "", fmt.Errorf("agent %s is not your direct continuable child or your direct parent", target)
	}
	message := core.Message{Role: "user", Content: []core.Block{{Type: "text", Text: "Agent " + sender + " sent a message: "}, {Type: "text", Text: text}}, Source: map[string]any{"kind": "agent", "agentId": sender}}
	return m.deliver(ctx, target, message, false)
}

// deliver commits a message for target. A running child admits it at its
// next step; an idle child or a top-level Session starts a turn with it.
// Settlement notices are kept even when no turn can start yet.
func (m *agents) deliver(ctx context.Context, target string, message core.Message, settlement bool) (string, error) {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return "", errManagerClosing
	}
	if c, ok := m.children[target]; ok {
		defer m.mu.Unlock()
		stopping := c.Status == "running" && c.ctx.Err() != nil
		full := c.Status != "running" && m.active >= m.max
		if !settlement && stopping {
			return "", errors.New("subagent is stopping; wait before sending a message")
		}
		if !settlement && full {
			return "", fmt.Errorf("maximum of %d active subagents reached", m.max)
		}
		// Committing under mu orders this against the child's end-of-turn check.
		id, err := m.app.engine.Inbox(ctx, target, message)
		if err == nil && c.Status != "running" && !full && !(settlement && (c.torndown || m.wakePaused[target])) {
			m.resumeLocked(c, nil)
		}
		return id, err
	}
	if m.wakePaused[target] {
		defer m.mu.Unlock()
		return m.app.engine.Inbox(ctx, target, message)
	}
	wakeCtx, wakeCancel := context.WithCancel(m.app.ctx)
	m.nextWake++
	wakeID := m.nextWake
	m.wakeRuns[wakeID] = &parentWake{target: target, cancel: wakeCancel}
	m.waking++
	m.wakeWG.Add(1)
	m.mu.Unlock()
	id, err := m.app.engine.Inbox(ctx, target, message)
	if err != nil {
		wakeCancel()
		m.mu.Lock()
		m.waking--
		delete(m.wakeRuns, wakeID)
		m.mu.Unlock()
		m.wakeWG.Done()
		return "", err
	}
	go func() {
		defer m.wakeWG.Done()
		defer wakeCancel()
		defer func() {
			m.mu.Lock()
			m.waking--
			delete(m.wakeRuns, wakeID)
			m.mu.Unlock()
		}()
		// A busy Session admits the message itself; an idle one is woken.
		if err := m.app.engine.ContinueInbox(wakeCtx, target); err != nil && !errors.Is(err, context.Canceled) {
			m.app.line("[agent %s could not continue: %s]\n", target, clean(err.Error()))
		}
	}()
	return id, nil
}

func (m *agents) notifySettled(id, parent, status, errText string) {
	defer func() {
		m.mu.Lock()
		m.waking--
		m.mu.Unlock()
	}()
	if _, err := m.deliver(m.app.ctx, parent, m.app.settlementMessage(id, status, errText), true); err != nil && !errors.Is(err, errManagerClosing) && m.app.ctx.Err() == nil {
		m.app.line("[subagent %s could not notify %s: %s]\n", id, parent, clean(err.Error()))
	}
}

// interruptOfficial stops a descendant's current work; its own descendants keep running.
func (m *agents) interruptOfficial(caller, target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.descendantsLocked(caller) {
		if id != target {
			continue
		}
		if c := m.children[id]; c.Status == "running" {
			c.interrupted = true
			c.queue = nil
			c.cancel()
		}
		return nil
	}
	return fmt.Errorf("agent %s is not a subagent started under you", target)
}

// catalogOfficial lists this process's children in start order: direct
// children, or every descendant depth-first with its parent and depth.
func (m *agents) catalogOfficial(ctx context.Context, parent string, descendants bool) ([]subagentCatalogEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := []subagentCatalogEntry{}
	var walk func(string, int)
	walk = func(id string, depth int) {
		var kids []*child
		for _, c := range m.children {
			if c.ParentID == id {
				kids = append(kids, c)
			}
		}
		sort.Slice(kids, func(i, j int) bool { return kids[i].seq < kids[j].seq })
		for _, c := range kids {
			row := subagentCatalogEntry{Kind: "child", ID: c.ID, Label: c.Label, Status: "inactive"}
			if c.Status == "running" {
				row.Status = "running"
			}
			if !descendants {
				rows = append(rows, row)
				continue
			}
			row.Parent, row.Depth = id, depth
			rows = append(rows, row)
			walk(c.ID, depth+1)
		}
	}
	walk(parent, 1)
	return rows, nil
}

// settle waits until parent has no running descendant or background job, no
// pending notice and no woken turn, so a one-shot run collects its work.
func (m *agents) settle(ctx context.Context, parent string) error {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		m.mu.Lock()
		busy := m.waking > 0
		owners := append(m.descendantsLocked(parent), parent)
		for _, id := range owners[:len(owners)-1] {
			busy = busy || m.children[id].Status == "running"
		}
		m.mu.Unlock()
		busy = busy || m.app.registry.HasPendingJobs(owners...) || m.app.workflowNotices.busy()
		if !busy && !m.app.engine.Busy(parent) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// settlementMessage is the parent's notice for a settled child: why it
// ended, its nonempty closing text and, after a failure, the diagnostic.
func (a *App) settlementMessage(id, status, errText string) core.Message {
	subject := "Background subagent " + id
	summary := subject + " finished and will do no further work unless you send it more."
	switch status {
	case "cancelled":
		summary = subject + " was stopped before it finished."
	case "failed":
		summary = subject + " failed before it finished."
		if strings.Contains(errText, "output limit reached") || strings.Contains(errText, "context window exceeded") {
			summary = subject + " ran out of room before it finished."
		}
	}
	blocks := []core.Block{{Type: "text", Text: summary}}
	if closing := a.closingText(id); len(closing) > 0 {
		blocks = append(append(blocks, core.Block{Type: "text", Text: "Its closing message:"}), closing...)
	} else {
		blocks = append(blocks, core.Block{Type: "text", Text: "It left no closing message."})
	}
	if status == "failed" && errText != "" {
		blocks = append(blocks, core.Block{Type: "text", Text: errText})
	}
	return core.Message{Role: "user", Content: blocks, Source: map[string]any{"kind": "subagent", "form": "notice", "childId": id}}
}

// closingText is the nonempty text of the last assistant message in the child's latest turn.
func (a *App) closingText(id string) []core.Block {
	events, err := a.store.Events(id)
	if err != nil {
		return nil
	}
	turn := ""
	var last []core.Block
	for _, ev := range events {
		switch ev.Type {
		case "turn/started":
			turn, last = ev.TurnID, nil
		case "assistant/message":
			var message core.Message
			if ev.TurnID != turn || json.Unmarshal(ev.Data, &message) != nil {
				continue
			}
			last = nil
			for _, b := range message.Content {
				if b.Type == "text" && b.Text != "" {
					last = append(last, core.Block{Type: "text", Text: b.Text})
				}
			}
		}
	}
	return last
}

// settleDelegations keeps a one-shot run alive until its children settle and
// their notices are handled; an interrupt cancels the whole tree.
func (a *App) settleDelegations() error {
	ctx, stop := signalContext(a.ctx)
	defer stop()
	err := a.manager.settle(ctx, a.session.ID)
	if err != nil && a.ctx.Err() == nil {
		a.manager.cancelChildren(a.session.ID)
		a.cancel()
	}
	return err
}
