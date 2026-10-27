package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"dsgo/internal/core"
)

func officialApp(t *testing.T) (*App, core.Session, func(name string, sessionID string, args map[string]any) (string, error)) {
	t.Helper()
	var parentID string
	a, parent := lifecycleApp(t, lifecycleModel(func(ctx context.Context, req core.Request, emit func(core.Delta)) (core.Response, error) {
		if req.SessionID == parentID {
			return core.Response{Content: []core.Block{{Type: "text", Text: "parent noted"}}}, nil
		}
		if lifecyclePrompt(req) == "block" {
			<-ctx.Done()
			return core.Response{}, ctx.Err()
		}
		return core.Response{Content: []core.Block{{Type: "text", Text: "child result"}}}, nil
	}))
	parentID = parent.ID
	call := func(name, sessionID string, args map[string]any) (string, error) {
		t.Helper()
		tc := core.ToolContext{SessionID: sessionID}
		res, err := a.Execute(a.ctx, tc, name, core.JSON(args))
		if err != nil {
			return "", err
		}
		res, err = a.spillToolResult(a.ctx, tc, name, res)
		return res.Content, err
	}
	return a, parent, call
}

func settleOfficial(t *testing.T, a *App, parent string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(a.ctx, 5*time.Second)
	defer cancel()
	if err := a.manager.settle(ctx, parent); err != nil {
		t.Fatalf("delegations did not settle: %v", err)
	}
}

// notices returns the parent's admitted user messages that runtime agents sent.
func notices(t *testing.T, a *App, parent string) []string {
	t.Helper()
	messages, err := a.store.Messages(parent)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range messages {
		if m.Role != "user" || len(m.Content) == 0 {
			continue
		}
		if first := m.Content[0].Text; strings.HasPrefix(first, "Background subagent ") || strings.HasPrefix(first, "Agent ") {
			var parts []string
			for _, b := range m.Content {
				parts = append(parts, b.Text)
			}
			out = append(out, strings.Join(parts, "|"))
		}
	}
	return out
}

func TestOfficialSubagentNoticesWakeParentAndFollowUpsResume(t *testing.T) {
	a, parent, call := officialApp(t)
	text, err := call("subagent", parent.ID, map[string]any{"description": "Check things", "prompt": "do work"})
	if err != nil || !strings.HasPrefix(text, "started subagent ") {
		t.Fatalf("start: %q %v", text, err)
	}
	id := strings.TrimPrefix(text, "started subagent ")
	settleOfficial(t, a, parent.ID)
	got := notices(t, a, parent.ID)
	want := "Background subagent " + id + " finished and will do no further work unless you send it more.|Its closing message:|child result"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("settlement notice: %q", got)
	}
	// The idle parent was woken and answered the notice.
	messages, _ := a.store.Messages(parent.ID)
	if last := messages[len(messages)-1]; last.Role != "assistant" || last.Content[0].Text != "parent noted" {
		t.Fatalf("parent was not woken: %+v", last)
	}
	child, _ := a.store.Messages(id)
	if len(child[0].Content) != 2 || !strings.Contains(child[0].Content[1].Text, `Your parent agent id is "`+parent.ID+`"`) {
		t.Fatalf("return guidance: %+v", child[0])
	}
	if text, _ = call("list_agents", parent.ID, map[string]any{}); text != id+" [inactive] — Check things" {
		t.Fatalf("list_agents: %q", text)
	}
	if text, err = call("send_message", parent.ID, map[string]any{"agent_id": id, "message": "again"}); err != nil || text != "message delivered to agent "+id {
		t.Fatalf("follow-up: %q %v", text, err)
	}
	settleOfficial(t, a, parent.ID)
	if got = notices(t, a, parent.ID); len(got) != 2 {
		t.Fatalf("follow-up did not settle with a second notice: %q", got)
	}
	child, _ = a.store.Messages(id)
	if first := child[len(child)-2].Content; first[0].Text != "Agent "+parent.ID+" sent a message: " || first[1].Text != "again" {
		t.Fatalf("adjacent message: %+v", first)
	}
	if _, err = call("send_message", parent.ID, map[string]any{"agent_id": "unknown", "message": "x"}); err == nil {
		t.Fatal("message to an unrelated agent was accepted")
	}
	// Default depth 1 lets children work but not delegate further.
	if _, err = call("subagent", id, map[string]any{"description": "Deeper", "prompt": "do work"}); err == nil || !strings.Contains(err.Error(), "delegation depth 1 reached maximum 1") {
		t.Fatalf("depth cap: %v", err)
	}
}

func TestOfficialInterruptKeepsDescendantsAndTeardownIsSilent(t *testing.T) {
	a, parent, call := officialApp(t)
	text, err := call("subagent", parent.ID, map[string]any{"description": "Long task", "prompt": "block"})
	if err != nil {
		t.Fatal(err)
	}
	childID := strings.TrimPrefix(text, "started subagent ")
	grandID, err := a.manager.spawn(childID, "block", "nested", "fresh")
	if err != nil {
		t.Fatal(err)
	}
	// A resident child can report to its direct parent, which is woken.
	if _, err = a.manager.sendOfficial(a.ctx, childID, parent.ID, "progress"); err != nil {
		t.Fatal(err)
	}
	if text, _ = call("list_agents", parent.ID, map[string]any{"scope": "descendants"}); text != childID+" [running] parent="+parent.ID+" depth=1 — Long task\n"+grandID+" [running] parent="+childID+" depth=2 — nested" {
		t.Fatalf("descendants: %q", text)
	}
	if text, err = call("interrupt_agent", parent.ID, map[string]any{"agent_id": childID}); err != nil || text != "interrupt requested for agent "+childID {
		t.Fatalf("interrupt: %q %v", text, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		a.manager.mu.Lock()
		child, grand := a.manager.children[childID].Status, a.manager.children[grandID].Status
		a.manager.mu.Unlock()
		if child == "cancelled" {
			if grand != "running" {
				t.Fatalf("interrupt reached a descendant: %s", grand)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("interrupted child did not stop")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = a.manager.interruptOfficial(parent.ID, "unknown"); err == nil {
		t.Fatal("interrupted an agent outside the caller's tree")
	}
	// Teardown cancels the rest without another notice.
	a.manager.cancelChildren(parent.ID)
	settleOfficial(t, a, parent.ID)
	got := notices(t, a, parent.ID)
	if len(got) != 2 || got[0] != "Agent "+childID+" sent a message: |progress" || got[1] != "Background subagent "+childID+" was stopped before it finished.|It left no closing message." {
		t.Fatalf("notices: %q", got)
	}
}

func TestOfficialSubagentModelSelectionPolicy(t *testing.T) {
	a, _, call := officialApp(t)
	a.cfg.SubagentModelSelection = json.RawMessage(`{"enabled":true,"allowedModels":[{"provider":"deepseek-official","model":"fixture-model"}]}`)
	s, err := a.store.Create(t.TempDir(), "selected")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.sampleSubagentPolicy(s.ID); err != nil {
		t.Fatal(err)
	}
	listed := map[string]core.ToolSpec{}
	for _, spec := range a.specsFor(s) {
		listed[spec.Name] = spec
	}
	if _, ok := listed["list_subagent_models"]; !ok || !strings.Contains(string(listed["subagent"].InputSchema), `"reasoning_effort"`) {
		t.Fatalf("selection tools not advertised: %v", listed["subagent"].InputSchema)
	}
	if _, err = call("subagent", s.ID, map[string]any{"description": "x", "prompt": "do work", "provider": "deepseek-official", "model": "other"}); err == nil || !strings.Contains(err.Error(), "is not allowed") {
		t.Fatalf("unapproved route: %v", err)
	}
	text, err := call("subagent", s.ID, map[string]any{"description": "x", "prompt": "do work", "provider": "deepseek-official", "model": "fixture-model", "reasoning_effort": "high"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := a.store.Get(strings.TrimPrefix(text, "started subagent "))
	if err != nil || child.Provider != "deepseek-official" || child.Model != "fixture-model" || child.Thinking != "high" {
		t.Fatalf("child route: %+v %v", child, err)
	}
	if policy, err := a.subagentPolicy(child.ID); err != nil || policy == nil {
		t.Fatalf("child did not inherit the route policy: %v", err)
	}
	settleOfficial(t, a, s.ID)
	// A Session without a recorded opt-in keeps the plain schema.
	plain, _ := a.store.Create(t.TempDir(), "plain")
	for _, spec := range a.specsFor(plain) {
		if spec.Name == "list_subagent_models" || (spec.Name == "subagent" && strings.Contains(string(spec.InputSchema), "provider")) {
			t.Fatalf("selection leaked to a Session without a policy: %s", spec.Name)
		}
	}
}
