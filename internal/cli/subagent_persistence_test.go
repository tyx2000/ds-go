package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"dsgo/internal/core"
)

func restartAgentManager(a *App) {
	a.manager.close()
	a.manager = newAgents(a)
}

func TestSubagentCatalogSurvivesRestartWithoutLoadingChildren(t *testing.T) {
	a, parent, call := officialApp(t)
	text, err := call("subagent", parent.ID, map[string]any{"description": "Durable child", "prompt": "work"})
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimPrefix(text, "started subagent ")
	settleOfficial(t, a, parent.ID)
	manual, err := a.store.Fork(parent.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	childMeta, err := a.store.Get(id)
	if err != nil || childMeta.CreationLabel != "Durable child" || manual.CreationLabel != "" {
		t.Fatalf("creation label ownership: child=%+v fork=%+v %v", childMeta, manual, err)
	}
	if _, err = a.store.CreateChild(parent.CWD, "not a subagent", parent.ID); err != nil {
		t.Fatal(err)
	}
	restartAgentManager(a)
	rows, err := a.manager.catalogOfficial(a.ctx, parent.ID, false)
	if err != nil || len(rows) != 1 || rows[0].ID != id || rows[0].Status != "inactive" || rows[0].Mode != "continuable" {
		t.Fatalf("cold catalog: %+v %v", rows, err)
	}
	if len(a.manager.children) != 0 {
		t.Fatal("discovery materialized a child")
	}
	rows, err = a.manager.catalogOfficial(a.ctx, manual.ID, true)
	if err != nil || len(rows) != 0 {
		t.Fatalf("manual fork inherited catalog ownership: %+v %v", rows, err)
	}
	// Direct discovery must remain available without opening this child log.
	path := filepath.Join(a.store.Root(), "sessions", id, "events.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("broken committed record\n")
	if closeErr := f.Close(); err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
	rows, err = a.manager.catalogOfficial(a.ctx, parent.ID, false)
	if err != nil || len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("direct catalog read child log: %+v %v", rows, err)
	}
	rows, err = a.manager.catalogOfficial(a.ctx, parent.ID, true)
	if err != nil || len(rows) != 1 || rows[0].Kind != "diagnostic" || rows[0].Reason != "corrupt" {
		t.Fatalf("recursive corruption diagnostic: %+v %v", rows, err)
	}
	if _, err = a.manager.sendOfficial(a.ctx, parent.ID, id, "follow"); err == nil {
		t.Fatal("cold-resumed corrupt child")
	}
	if len(a.manager.children) != 0 || a.manager.active != 0 {
		t.Fatal("failed restoration retained admission")
	}
}

func TestSubagentColdResumeSerializesMessagesAndRetainsDeclaredScope(t *testing.T) {
	var parentID string
	var concurrent, maxConcurrent atomic.Int32
	gate := make(chan struct{})
	started := make(chan struct{}, 32)
	var once sync.Once
	defer once.Do(func() { close(gate) })
	a, parent := lifecycleApp(t, lifecycleModel(func(ctx context.Context, req core.Request, _ func(core.Delta)) (core.Response, error) {
		if req.SessionID == parentID {
			return core.Response{Content: []core.Block{{Type: "text", Text: "noted"}}}, nil
		}
		n := concurrent.Add(1)
		defer concurrent.Add(-1)
		for old := maxConcurrent.Load(); n > old; old = maxConcurrent.Load() {
			if maxConcurrent.CompareAndSwap(old, n) {
				break
			}
		}
		if strings.HasPrefix(lifecyclePrompt(req), "Agent ") {
			if req.Provider != "fixture-provider" || req.Model != "fixture-model" || req.Thinking != "low" {
				return core.Response{}, fmt.Errorf("changed cold route: %s/%s/%s", req.Provider, req.Model, req.Thinking)
			}
			started <- struct{}{}
			select {
			case <-gate:
			case <-ctx.Done():
				return core.Response{}, ctx.Err()
			}
		}
		return core.Response{Content: []core.Block{{Type: "text", Text: "child result"}}}, nil
	}))
	parentID = parent.ID
	id, err := a.manager.spawnOfficial(a.ctx, core.ToolContext{SessionID: parent.ID, CWD: parent.CWD}, subagentStart{Prompt: "initial", Description: "Restorable", Mode: "fresh"}, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	settleOfficial(t, a, parent.ID)
	restartAgentManager(a)
	if len(notices(t, a, parent.ID)) != 1 {
		t.Fatal("restart replayed settlement")
	}
	// Later parent settings must not replace the child's declared route/scope.
	parent.Provider, parent.Model, parent.Thinking, parent.Permission = "new-provider", "new-model", "high", "full-access"
	if err = a.store.Update(parent); err != nil {
		t.Fatal(err)
	}
	var sends sync.WaitGroup
	for i := range 16 {
		sends.Add(1)
		go func() {
			defer sends.Done()
			if _, err := a.manager.sendOfficial(a.ctx, parent.ID, id, fmt.Sprintf("follow-%d", i)); err != nil {
				t.Errorf("send: %v", err)
			}
		}()
	}
	sends.Wait()
	<-started
	a.manager.mu.Lock()
	if a.manager.active != 1 || len(a.manager.children) != 1 {
		t.Errorf("duplicate cold activation: active=%d children=%d", a.manager.active, len(a.manager.children))
	}
	a.manager.mu.Unlock()
	once.Do(func() { close(gate) })
	settleOfficial(t, a, parent.ID)
	if maxConcurrent.Load() != 1 {
		t.Fatalf("overlapping child requests: %d", maxConcurrent.Load())
	}
	child, err := a.store.Get(id)
	if err != nil || child.Permission != "read-only" {
		t.Fatalf("scope changed: %+v %v", child, err)
	}
	messages, err := a.store.Messages(id)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		for _, block := range message.Content {
			counts[block.Text]++
		}
	}
	if counts["initial"] != 1 {
		t.Fatal("cold resume replayed original task")
	}
	for i := range 16 {
		if counts[fmt.Sprintf("follow-%d", i)] != 1 {
			t.Errorf("message %d not admitted exactly once: %v", i, counts)
		}
	}
	events, _ := a.store.Events(id)
	descriptors := 0
	for _, event := range events {
		if event.Type == "subagent/descriptor" {
			descriptors++
		}
	}
	if descriptors != 1 || len(notices(t, a, parent.ID)) != 2 {
		t.Fatal("cold resume duplicated descriptor or settlement")
	}
}

func TestSubagentColdResumeRejectsUnownedCancelledAndFullAdmissions(t *testing.T) {
	a, parent, call := officialApp(t)
	text, err := call("subagent", parent.ID, map[string]any{"description": "Durable", "prompt": "work"})
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimPrefix(text, "started subagent ")
	settleOfficial(t, a, parent.ID)
	restartAgentManager(a)
	other, _ := a.store.Create(parent.CWD, "unrelated")
	if _, err = a.manager.sendOfficial(a.ctx, other.ID, id, "wrong owner"); err == nil {
		t.Fatal("unrelated parent resumed child")
	}
	ctx, cancel := context.WithCancel(a.ctx)
	cancel()
	if _, err = a.manager.sendOfficial(ctx, parent.ID, id, "cancelled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled send: %v", err)
	}
	a.manager.max = 1
	if _, err = a.manager.spawn(parent.ID, "block", "occupy capacity", "fresh"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.manager.sendOfficial(a.ctx, parent.ID, id, "over capacity"); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("capacity did not bound cold resume: %v", err)
	}
	if _, ok := a.manager.children[id]; ok {
		t.Fatal("refused cold resume left resident state")
	}
}

func TestSubagentDescriptorExcludesInheritedFactsAndRejectsRewrite(t *testing.T) {
	inherited := core.Event{Type: "subagent/descriptor", Data: core.JSON(subagentDescriptor{Version: 3, Mode: "continuable", Provider: "spawn", Label: "parent"})}
	own := subagentDescriptor{Version: 3, Mode: "continuable", Provider: "fork", Label: "child", AgentModel: "original"}
	events := []core.Event{inherited, {Type: "session/forked"}, {Type: "subagent/descriptor", Data: core.JSON(own)}, {Type: "subagent/delegated-policy", Data: core.JSON(delegatedPolicy{Version: 1, Permission: "read-only"})}, {Type: "subagent/descriptor", Data: json.RawMessage(`{"version":3,"mode":"continuable","provider":"spawn","label":"rewrite"}`)}}
	desc, _, err := readChildDescriptor(events)
	if err != nil || desc.Label != "child" || desc.AgentModel != "original" {
		t.Fatalf("inherited or rewritten descriptor won: %+v %v", desc, err)
	}
}

func TestDelegatedScopeRejectsApprovalAndAppearsInPrompt(t *testing.T) {
	a, parent, _ := officialApp(t)
	child, err := a.store.CreateChild(parent.CWD, "scope", parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	child.Permission = "read-only"
	if err = a.store.Update(child); err != nil {
		t.Fatal(err)
	}
	called := false
	err = a.registry.Register(toolSpec("check_scope", "check", `{"type":"object"}`), func(_ context.Context, tc core.ToolContext, _ json.RawMessage) (core.ToolResult, error) {
		called = true
		if tc.Permission != "read-only" || tc.ApprovalPolicy != "never" || tc.Approve != nil {
			t.Fatalf("delegated scope widened: %+v", tc)
		}
		return core.ToolResult{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.Execute(a.ctx, core.ToolContext{SessionID: child.ID, Permission: "full-access", ApprovalPolicy: "on-request", Approve: func(context.Context, core.Approval) (bool, error) {
		t.Fatal("child requested approval")
		return false, nil
	}}, "check_scope", core.JSON(map[string]any{})); err != nil || !called {
		t.Fatalf("scope tool: %v", err)
	}
	for _, preset := range []string{"standard", "minimal"} {
		child.Preset = preset
		messages, err := a.requestContext(a.ctx, child)
		if err != nil || len(messages) != 1 || messages[0].Role != "user" || !strings.Contains(messages[0].Content[0].Text, "permission scope was fixed") {
			t.Fatalf("%s delegated context: %+v %v", preset, messages, err)
		}
	}
}

func TestSubagentPresetSelectionIsOptInAndColdRestorable(t *testing.T) {
	a, parent, _ := officialApp(t)
	if strings.Contains(string(a.subagentSpecFor(parent, false).InputSchema), "agent_preset") {
		t.Fatal("default tool exposes an unconfigured preset choice")
	}
	a.cfg.SubagentAgentPreset = "minimal"
	a.cfg.SubagentAllowedAgentPresets = []string{"minimal", "ptc"}
	if !strings.Contains(string(a.subagentSpecFor(parent, false).InputSchema), `"agent_preset"`) || strings.Contains(string(a.subagentSpecFor(parent, true).InputSchema), `"agent_preset"`) {
		t.Fatal("preset enum is not confined to configured spawn")
	}
	start := func(raw string, fork bool) (string, error) {
		res, err := a.delegateOfficial(a.ctx, core.ToolContext{SessionID: parent.ID, CWD: parent.CWD}, json.RawMessage(raw), fork)
		if err != nil {
			return "", err
		}
		var activation struct {
			ID string `json:"subagentId"`
		}
		if err := json.Unmarshal([]byte(res.Content), &activation); err != nil {
			t.Fatal(err)
		}
		return activation.ID, nil
	}
	for _, args := range []string{`{"description":"x","prompt":"work","agent_preset":"standard"}`, `{"description":"x","prompt":"work","agent_preset":null}`} {
		if _, err := start(args, false); err == nil {
			t.Fatalf("accepted unconfigured or null preset: %s", args)
		}
	}
	if _, err := start(`{"description":"x","prompt":"work","agent_preset":"minimal"}`, true); err == nil {
		t.Fatal("fork accepted independent preset")
	}
	id, err := start(`{"description":"independent","prompt":"work"}`, false)
	if err != nil {
		t.Fatal(err)
	}
	settleOfficial(t, a, parent.ID)
	child, _ := a.store.Get(id)
	if child.Preset != "minimal" || child.ExecutionMode != "native" {
		t.Fatalf("configured independent preset not composed: %+v", child)
	}
	a.cfg.SubagentAgentPreset, a.cfg.SubagentAllowedAgentPresets = "", nil
	inheritedID, err := start(`{"description":"inherited","prompt":"work"}`, false)
	if err != nil {
		t.Fatal(err)
	}
	settleOfficial(t, a, parent.ID)
	restartAgentManager(a)
	parent.Preset, parent.ExecutionMode = "ptc", "ptc"
	if err = a.store.Update(parent); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{id, inheritedID} {
		if _, err := a.manager.sendOfficial(a.ctx, parent.ID, id, "follow"); err != nil {
			t.Fatal(err)
		}
	}
	settleOfficial(t, a, parent.ID)
	independent, _ := a.store.Get(id)
	inherited, _ := a.store.Get(inheritedID)
	if independent.Preset != "minimal" || inherited.Preset != "ptc" || inherited.ExecutionMode != "ptc" {
		t.Fatalf("cold composition: independent=%+v inherited=%+v", independent, inherited)
	}
}

func TestSubagentDepthContextUsesDurableFloorAndCurrentCap(t *testing.T) {
	a, parent, _ := officialApp(t)
	id, err := a.manager.spawn(parent.ID, "work", "depth", "fresh")
	if err != nil {
		t.Fatal(err)
	}
	a.manager.mu.Lock()
	done := a.manager.children[id].done
	a.manager.mu.Unlock()
	lifecycleDone(t, done)
	manual, err := a.store.Fork(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if manual.CreationLabel != "" {
		t.Fatal("manual fork inherited subagent creation label")
	}
	if depth, err := a.subagentDepth(manual.ID); err != nil || depth != 0 {
		t.Fatalf("manual fork inherited its source's recursion floor: %d %v", depth, err)
	}
	// A cold reader preserves the original child's own floor independently
	// of whether that child currently has a runtime owner.
	if depth, err := (&App{store: a.store}).subagentDepth(id); err != nil || depth != 1 {
		t.Fatalf("cold reader reset original child depth: %d %v", depth, err)
	}
	child, _ := a.store.Get(id)
	messages, err := a.requestContext(a.ctx, child)
	if err != nil || len(messages) != 1 || strings.Count(messages[0].Content[0].Text, "remaining levels 0") != 2 {
		t.Fatalf("depth context: %+v %v", messages, err)
	}
	cap := 3
	a.cfg.SubagentMaxDepth = &cap
	messages, err = a.requestContext(a.ctx, child)
	if err != nil || len(messages) != 1 || strings.Contains(messages[0].Content[0].Text, "remaining levels 0") {
		t.Fatalf("depth context ignored changed cap: %+v %v", messages, err)
	}
	child.Preset = "minimal"
	cap = 1
	messages, err = a.requestContext(a.ctx, child)
	if err != nil || len(messages) != 1 || strings.Contains(messages[0].Content[0].Text, "Delegation depth") {
		t.Fatalf("hidden delegation tool contributed depth context: %+v %v", messages, err)
	}
}

func TestSubagentManualForkResetsDepthButDelegatedForkIncrements(t *testing.T) {
	a, parent, _ := officialApp(t)
	first, err := a.manager.spawn(parent.ID, "work", "first", "fresh")
	if err != nil {
		t.Fatal(err)
	}
	a.manager.mu.Lock()
	done := a.manager.children[first].done
	a.manager.mu.Unlock()
	lifecycleDone(t, done)
	delegatedFork, err := a.manager.spawn(first, "work", "delegated fork", "fork")
	if err != nil {
		t.Fatal(err)
	}
	a.manager.mu.Lock()
	done = a.manager.children[delegatedFork].done
	a.manager.mu.Unlock()
	lifecycleDone(t, done)
	if depth, err := a.subagentDepth(delegatedFork); err != nil || depth != 2 {
		t.Fatalf("delegated fork did not retain its own new floor: %d %v", depth, err)
	}
	manual, err := a.store.Fork(delegatedFork, 0)
	if err != nil {
		t.Fatal(err)
	}
	if depth, err := a.subagentDepth(manual.ID); err != nil || depth != 0 {
		t.Fatalf("manual fork inherited depth 2: %d %v", depth, err)
	}
	fresh, err := a.manager.spawn(manual.ID, "work", "new root child", "fresh")
	if err != nil {
		t.Fatal(err)
	}
	a.manager.mu.Lock()
	done = a.manager.children[fresh].done
	a.manager.mu.Unlock()
	lifecycleDone(t, done)
	if depth, err := a.subagentDepth(fresh); err != nil || depth != 1 {
		t.Fatalf("manual root child did not start a new recursion count: %d %v", depth, err)
	}
}

func TestSubagentStoredDepthRejectsUnsafeIntegerAndOverflow(t *testing.T) {
	a, parent, _ := officialApp(t)
	if _, err := a.store.Append(parent.ID, "", "subagent/delegation-depth", core.JSON(map[string]any{"version": 1, "depth": int64(9007199254740992)})); err != nil {
		t.Fatal(err)
	}
	if _, err := a.subagentDepth(parent.ID); err == nil {
		t.Fatal("accepted a non-safe stored depth")
	}
	if _, err := a.store.Append(parent.ID, "", "subagent/delegation-depth", core.JSON(map[string]any{"version": 1, "depth": int64(9007199254740991)})); err != nil {
		t.Fatal(err)
	}
	legacy, err := a.store.CreateChild(parent.CWD, "legacy child", parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	legacy.Delegated = true
	if err := a.store.Update(legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := a.subagentDepth(legacy.ID); err == nil {
		t.Fatal("legacy lineage overflowed the recursion floor")
	}
}

func TestSubagentStoredFactsRejectNullRequiredFields(t *testing.T) {
	for _, raw := range []string{
		`{"version":null,"childId":"child","childCreatedAt":1,"mode":"continuable","label":"x"}`,
		`{"version":0,"childId":"child","mode":"continuable","label":"x"}`,
		`{"version":0,"childId":"child","childCreatedAt":1,"mode":"continuable","label":null}`,
	} {
		t.Run(raw, func(t *testing.T) {
			a, parent, _ := officialApp(t)
			if _, err := a.store.Append(parent.ID, "", "subagent/catalog", json.RawMessage(raw)); err != nil {
				t.Fatal(err)
			}
			if _, err := a.manager.readCatalog(a.ctx, parent.ID); err == nil {
				t.Fatal("accepted invalid catalog")
			}
		})
	}
	for _, raw := range []string{
		`{"version":3,"mode":"continuable","provider":"spawn"}`,
		`{"version":3,"mode":"continuable","provider":"spawn","label":"x","agentModel":null}`,
	} {
		if _, _, err := readChildDescriptor([]core.Event{{Type: "subagent/descriptor", Data: json.RawMessage(raw)}, {Type: "subagent/delegated-policy", Data: core.JSON(delegatedPolicy{Version: 1, Permission: "read-only"})}}); err == nil {
			t.Fatal("accepted invalid descriptor")
		}
	}
}

func TestSubagentColdResumeRetainsLaterNarrowerPermission(t *testing.T) {
	a, parent, call := officialApp(t)
	parent.Permission = "full-access"
	if err := a.store.Update(parent); err != nil {
		t.Fatal(err)
	}
	text, err := call("subagent", parent.ID, map[string]any{"description": "scope", "prompt": "work"})
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimPrefix(text, "started subagent ")
	settleOfficial(t, a, parent.ID)
	child, _ := a.store.Get(id)
	child.Permission = "read-only"
	if err = a.store.Update(child); err != nil {
		t.Fatal(err)
	}
	restartAgentManager(a)
	if _, err := a.manager.sendOfficial(a.ctx, parent.ID, id, "follow"); err != nil {
		t.Fatal(err)
	}
	settleOfficial(t, a, parent.ID)
	child, _ = a.store.Get(id)
	if child.Permission != "read-only" {
		t.Fatal("cold restoration widened later user permission")
	}
}

func TestStructuredCompletionWaitsForOwnedStartsAndCleanup(t *testing.T) {
	a, parent := lifecycleApp(t, lifecycleModel(func(ctx context.Context, _ core.Request, _ func(core.Delta)) (core.Response, error) {
		<-ctx.Done()
		return core.Response{}, ctx.Err()
	}))
	other, err := a.store.Create(parent.CWD, "independent tree")
	if err != nil {
		t.Fatal(err)
	}
	reserved, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	started := make(chan string, 1)
	go func() {
		id, err := a.manager.spawnPrepared(a.ctx, parent.ID, "block", "owned", "fresh", func(string) error {
			close(reserved)
			<-release
			return nil
		})
		if err != nil {
			t.Error(err)
		}
		started <- id
	}()
	<-reserved
	if a.manager.canCompleteStructured(parent.ID) || !a.manager.canCompleteStructured(other.ID) {
		t.Fatal("pending materialization was missed or blocked an unrelated tree")
	}
	once.Do(func() { close(release) })
	id := <-started
	if id == "" || a.manager.canCompleteStructured(parent.ID) {
		t.Fatal("published child was missed")
	}
	if err := a.manager.cancelAndDrain(a.ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	if !a.manager.canCompleteStructured(parent.ID) {
		t.Fatal("finished cleanup retained structured completion ownership")
	}
}

func TestSubagentCapacityIsSharedOnlyWithinActivationTree(t *testing.T) {
	a, parent := lifecycleApp(t, lifecycleModel(func(ctx context.Context, _ core.Request, _ func(core.Delta)) (core.Response, error) {
		<-ctx.Done()
		return core.Response{}, ctx.Err()
	}))
	a.manager.max = 1
	childID, err := a.manager.spawn(parent.ID, "block", "first root", "fresh")
	if err != nil {
		t.Fatal(err)
	}
	other, err := a.store.Create(parent.CWD, "second root")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.manager.spawn(other.ID, "block", "second root child", "fresh"); err != nil {
		t.Fatalf("unrelated root consumed this pool: %v", err)
	}
	if _, err = a.manager.spawn(childID, "block", "same activation tree", "fresh"); err == nil {
		t.Fatal("resident descendant bypassed its root pool")
	}
	if _, err = a.manager.spawn(parent.ID, "block", "same root sibling", "fresh"); err == nil {
		t.Fatal("sibling bypassed root pool")
	}
	if err = a.manager.cancelAndDrain(a.ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = a.manager.spawn(parent.ID, "block", "still full", "fresh"); err == nil {
		t.Fatal("unrelated pool disposal released this pool's slot")
	}
}
