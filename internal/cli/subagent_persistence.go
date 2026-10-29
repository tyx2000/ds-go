package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"dsgo/internal/core"
)

// These model-hidden facts mirror the pinned subagent descriptor/catalog
// boundaries. Membership belongs to the parent; a child's own descriptor owns
// continuation. Neither a shared ParentID nor an inherited fork fact grants it.
type subagentDescriptor struct {
	Version              int    `json:"version"`
	Mode                 string `json:"mode"`
	Provider             string `json:"provider"`
	Label                string `json:"label"`
	AgentProvider        string `json:"agentProvider,omitempty"`
	AgentModel           string `json:"agentModel,omitempty"`
	AgentReasoningEffort string `json:"agentReasoningEffort,omitempty"`
}

type subagentCatalogFact struct {
	Version        int    `json:"version"`
	ChildID        string `json:"childId"`
	ChildCreatedAt int64  `json:"childCreatedAt"`
	Mode           string `json:"mode"`
	Label          string `json:"label,omitempty"`
}

type delegatedPolicy struct {
	Version    int    `json:"version"`
	Permission string `json:"permission"`
}

// A fork marker follows its inherited prefix. Looking only after the last
// marker prevents a parent's descriptor and catalog from becoming the child's.
func ownSubagentEvents(events []core.Event) []core.Event {
	start := 0
	for i, event := range events {
		if event.Type == "session/forked" {
			start = i + 1
		}
	}
	return events[start:]
}

func requireStoredFields(raw json.RawMessage, fields ...string) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return errors.New("stored subagent fact must be an object")
	}
	for _, field := range fields {
		value, ok := object[field]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("stored subagent fact requires %s", field)
		}
	}
	for _, field := range []string{"label", "agentProvider", "agentModel", "agentReasoningEffort"} {
		if value, ok := object[field]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("stored subagent fact %s cannot be null", field)
		}
	}
	return nil
}

func (m *agents) describeChild(id, label, mode string) (subagentDescriptor, error) {
	s, err := m.app.store.Get(id)
	if err != nil {
		return subagentDescriptor{}, err
	}
	provider := "spawn"
	if mode == "fork" {
		provider = "fork"
	}
	desc := subagentDescriptor{Version: 3, Mode: "continuable", Provider: provider, Label: label, AgentProvider: s.Provider, AgentModel: s.Model, AgentReasoningEffort: s.Thinking}
	if _, err = m.app.store.Append(id, "", "subagent/descriptor", desc); err != nil {
		return subagentDescriptor{}, err
	}
	_, err = m.app.store.Append(id, "", "subagent/delegated-policy", delegatedPolicy{Version: 1, Permission: s.Permission})
	if err == nil {
		depth, depthErr := m.app.subagentDepth(s.ParentID)
		if depthErr != nil {
			return desc, depthErr
		}
		_, err = m.app.store.Append(id, "", "subagent/delegation-depth", map[string]any{"version": 1, "depth": depth + 1})
	}
	return desc, err
}

func validChildPreset(id string) bool {
	return id == "standard" || id == "ptc" || id == "minimal" || id == "cordis"
}

func independentChildPreset(events []core.Event) (string, bool, error) {
	for _, event := range ownSubagentEvents(events) {
		if event.Type == "subagent/descriptor" {
			break
		}
		if event.Type != "agent-preset/selected" {
			continue
		}
		var selection struct {
			AgentPreset *string `json:"agentPreset"`
		}
		if err := decodeOfficialObject(event.Data, &selection); err != nil || selection.AgentPreset == nil || !validChildPreset(*selection.AgentPreset) {
			return "", false, errors.New("selected child preset is unavailable")
		}
		return *selection.AgentPreset, true, nil
	}
	return "", false, nil
}

func (m *agents) establishCatalogChild(parent string, session core.Session, desc subagentDescriptor) error {
	_, err := m.app.store.Append(parent, "", "subagent/catalog", subagentCatalogFact{Version: 0, ChildID: session.ID, ChildCreatedAt: session.CreatedAt.UnixMilli(), Mode: desc.Mode, Label: desc.Label})
	return err
}

// readCatalog never opens child logs. Old dsgo start facts remain discoverable
// as unknown entries, without inferring a continuation contract they never saved.
func (m *agents) readCatalog(ctx context.Context, parent string) ([]subagentCatalogFact, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := m.app.store.Get(parent); err != nil {
		return nil, err
	}
	events, err := m.app.store.Events(parent)
	if err != nil {
		return nil, err
	}
	events = ownSubagentEvents(events)
	known := map[string]bool{}
	for _, event := range events {
		if event.Type == "subagent/catalog" {
			var fact subagentCatalogFact
			if err := requireStoredFields(event.Data, "version", "childId", "childCreatedAt", "mode"); err != nil {
				return nil, fmt.Errorf("corrupt subagent catalog: %w", err)
			}
			if err := decodeOfficialObject(event.Data, &fact); err != nil {
				return nil, fmt.Errorf("corrupt subagent catalog: %w", err)
			}
			if fact.ChildID == "" || fact.ChildCreatedAt < 0 || fact.Version < 0 || fact.Version > 2 ||
				!((fact.Version <= 1 && (fact.Mode == "continuable" || fact.Mode == "one-shot")) ||
					(fact.Version == 1 && fact.Mode == "unknown") || (fact.Version == 2 && fact.Mode == "external")) {
				return nil, errors.New("corrupt subagent catalog entry")
			}
			if fact.Mode == "continuable" {
				if err := requireStoredFields(event.Data, "label"); err != nil {
					return nil, fmt.Errorf("corrupt subagent catalog: %w", err)
				}
			}
			known[fact.ChildID] = true
		}
	}
	seen := map[string]bool{}
	rows := []subagentCatalogFact{}
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var fact subagentCatalogFact
		switch event.Type {
		case "subagent/catalog":
			_ = json.Unmarshal(event.Data, &fact) // validated in the first pass
		case "subagent/started":
			var old struct{ ID, Label string }
			if err := json.Unmarshal(event.Data, &old); err != nil || old.ID == "" || known[old.ID] {
				continue
			}
			fact = subagentCatalogFact{Version: 1, ChildID: old.ID, Label: old.Label, Mode: "unknown"}
		default:
			continue
		}
		if !seen[fact.ChildID] {
			seen[fact.ChildID] = true
			rows = append(rows, fact)
		}
	}
	return rows, ctx.Err()
}

func readChildDescriptor(events []core.Event) (subagentDescriptor, delegatedPolicy, error) {
	var desc subagentDescriptor
	var policy delegatedPolicy
	found := false
	for _, event := range ownSubagentEvents(events) {
		switch event.Type {
		case "subagent/descriptor":
			if found {
				continue // the first own descriptor is authoritative
			}
			found = true
			if err := requireStoredFields(event.Data, "version", "mode", "provider", "label"); err != nil {
				return desc, policy, err
			}
			if err := decodeOfficialObject(event.Data, &desc); err != nil || desc.Version != 3 || desc.Mode != "continuable" || desc.Provider == "" {
				return desc, policy, errors.New("subagent has no supported continuation descriptor")
			}
		case "subagent/delegated-policy":
			if policy.Version != 0 {
				continue
			}
			if err := requireStoredFields(event.Data, "version", "permission"); err != nil {
				return desc, policy, err
			}
			if err := decodeOfficialObject(event.Data, &policy); err != nil || policy.Version != 1 || (policy.Permission != "read-only" && policy.Permission != "workspace-write" && policy.Permission != "full-access") {
				return desc, policy, errors.New("invalid stored subagent permission scope")
			}
		}
	}
	if !found || policy.Version != 1 {
		return desc, policy, errors.New("subagent has no supported continuation state")
	}
	return desc, policy, nil
}

// restoreChildLocked materializes no work itself. The caller retains mu until
// inbox acceptance and resumeLocked, serializing concurrent sends and admission
// against close, subtree disposal and the active capacity limit.
func (m *agents) restoreChildLocked(ctx context.Context, parent, id string) (*child, error) {
	if m.closing || m.app.ctx.Err() != nil {
		return nil, errManagerClosing
	}
	if m.isDrainingLocked(parent) {
		return nil, errors.New("subagent subtree is being disposed")
	}
	if m.activeInTreeLocked(parent) >= m.max {
		return nil, fmt.Errorf("maximum of %d active subagents reached", m.max)
	}
	facts, err := m.readCatalog(ctx, parent)
	if err != nil {
		return nil, err
	}
	authorized := false
	for _, fact := range facts {
		if fact.ChildID == id && fact.Mode == "continuable" {
			authorized = true
			break
		}
	}
	if !authorized {
		return nil, fmt.Errorf("agent %s is not your direct continuable child", id)
	}
	session, err := m.app.store.Get(id)
	if err != nil {
		return nil, fmt.Errorf("subagent %s is unavailable: %w", id, err)
	}
	if session.ParentID != parent || !session.Delegated || session.Archived {
		return nil, errors.New("subagent is not an available direct child")
	}
	events, err := m.app.store.Events(id)
	if err != nil {
		return nil, fmt.Errorf("subagent %s has corrupt continuation state: %w", id, err)
	}
	desc, policy, err := readChildDescriptor(events)
	if err != nil {
		return nil, err
	}
	preset, independent, err := independentChildPreset(events)
	if err != nil {
		return nil, err
	}
	if independent {
		session.Preset, session.ExecutionMode = preset, "native"
		if preset == "ptc" {
			session.ExecutionMode = "ptc"
		}
	} else {
		parentSession, err := m.app.store.Get(parent)
		if err != nil {
			return nil, err
		}
		session.Preset, session.ExecutionMode = parentSession.Preset, parentSession.ExecutionMode
	}
	if _, err = m.app.subagentDepth(id); err != nil {
		return nil, err
	}
	if _, err = m.app.subagentPolicy(id); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	// Reconstruct the declared route and permission scope, never the parent's
	// current values. Working directory and session-local plan remain durable.
	session.Provider, session.Model, session.Thinking = desc.AgentProvider, desc.AgentModel, desc.AgentReasoningEffort
	session.CreationLabel = desc.Label
	permissionRank := map[string]int{"read-only": 1, "workspace-write": 2, "full-access": 3}
	if session.Permission == "" || permissionRank[session.Permission] > permissionRank[policy.Permission] {
		session.Permission = policy.Permission
	}
	if permissionRank[session.Permission] == 0 {
		return nil, errors.New("invalid stored current subagent permission scope")
	}
	if err = m.app.store.Update(session); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	close(done)
	childCtx, cancel := context.WithCancel(m.app.ctx)
	m.seq++
	c := &child{ID: id, ParentID: parent, Label: desc.Label, Status: "completed", done: done, cancel: cancel, ctx: childCtx, outcome: &childOutcome{done: done}, seq: m.seq, official: true}
	m.children[id] = c
	return c, nil
}
