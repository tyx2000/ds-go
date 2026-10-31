package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dsgo/internal/agent"
	"dsgo/internal/core"
	"dsgo/internal/state"
	"dsgo/internal/tools"
)

type lifecycleModel func(context.Context, core.Request, func(core.Delta)) (core.Response, error)

func (f lifecycleModel) Stream(ctx context.Context, req core.Request, emit func(core.Delta)) (core.Response, error) {
	return f(ctx, req, emit)
}

func lifecycleApp(t *testing.T, model core.Model) (*App, core.Session) {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.Create(t.TempDir(), "parent")
	if err != nil {
		t.Fatal(err)
	}
	parent.Permission, parent.Model, parent.Thinking, parent.Provider = "read-only", "fixture-model", "low", "fixture-provider"
	if err = store.Update(parent); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &App{ctx: ctx, cancel: cancel, store: store, cfg: state.DefaultConfig(), out: io.Discard, err: io.Discard, registry: tools.New()}
	a.current.Store(parent.ID)
	a.engine = agent.New(store, model, a, agent.Options{Model: "fixture-model"})
	a.manager = newAgents(a)
	if err = a.registerTools(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.manager.close(); a.engine.Close(); _ = a.registry.Close(); cancel() })
	return a, parent
}

func lifecycleDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("subagent did not settle")
	}
}

func lifecyclePrompt(req core.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			for _, b := range req.Messages[i].Content {
				if b.Type == "text" {
					return b.Text
				}
			}
		}
	}
	return ""
}

func TestSubagentConcurrentSpawnsReserveSlots(t *testing.T) {
	started := make(chan struct{}, 16)
	a, parent := lifecycleApp(t, lifecycleModel(func(ctx context.Context, req core.Request, emit func(core.Delta)) (core.Response, error) {
		started <- struct{}{}
		<-ctx.Done()
		return core.Response{}, ctx.Err()
	}))
	a.manager.max = 2
	var attempts sync.WaitGroup
	var accepted atomic.Int32
	for i := 0; i < 24; i++ {
		attempts.Add(1)
		go func() {
			defer attempts.Done()
			if _, err := a.manager.spawn(parent.ID, "work", "child", "fresh"); err == nil {
				accepted.Add(1)
			}
		}()
	}
	attempts.Wait()
	if got := accepted.Load(); got != 2 {
		t.Fatalf("accepted %d concurrent agents with limit 2", got)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("reserved agent never started")
		}
	}
	a.manager.close()
	a.manager.mu.Lock()
	defer a.manager.mu.Unlock()
	if a.manager.active != 0 {
		t.Fatalf("leaked %d active reservations", a.manager.active)
	}
}

func TestSubagentFollowupsQueueAndReopenAtomically(t *testing.T) {
	firstEntered, releaseFirst := make(chan struct{}), make(chan struct{})
	var first sync.Once
	a, parent := lifecycleApp(t, lifecycleModel(func(ctx context.Context, req core.Request, emit func(core.Delta)) (core.Response, error) {
		if lifecyclePrompt(req) == "first" {
			first.Do(func() { close(firstEntered) })
			select {
			case <-releaseFirst:
			case <-ctx.Done():
				return core.Response{}, ctx.Err()
			}
		}
		return core.Response{Content: []core.Block{{Type: "text", Text: "answer " + lifecyclePrompt(req)}}}, nil
	}))
	id, err := a.manager.spawn(parent.ID, "first", "child", "fresh")
	if err != nil {
		t.Fatal(err)
	}
	lifecycleDone(t, firstEntered)
	if _, err = a.agentMessage(a.ctx, core.ToolContext{SessionID: parent.ID}, core.JSON(map[string]string{"id": id, "message": "second"})); err != nil {
		t.Fatal(err)
	}
	a.manager.mu.Lock()
	oldOutcome := a.manager.children[id].outcome
	a.manager.mu.Unlock()
	close(releaseFirst)
	lifecycleDone(t, oldOutcome.done)
	if oldOutcome.status != "completed" || len(oldOutcome.messages) != 4 {
		t.Fatalf("queued transcript: status=%s messages=%d", oldOutcome.status, len(oldOutcome.messages))
	}
	// A later run must use another completion object, and cannot close or alter
	// the completion snapshot already observed by waiters of the first run.
	if _, err = a.agentMessage(a.ctx, core.ToolContext{SessionID: parent.ID}, core.JSON(map[string]string{"id": id, "message": "third"})); err != nil {
		t.Fatal(err)
	}
	a.manager.mu.Lock()
	newOutcome := a.manager.children[id].outcome
	a.manager.mu.Unlock()
	if newOutcome == oldOutcome {
		t.Fatal("follow-up reused a completion channel")
	}
	lifecycleDone(t, newOutcome.done)
	if len(oldOutcome.messages) != 4 || len(newOutcome.messages) != 6 {
		t.Fatal("settled transcript changed during later follow-up")
	}
	for i, prompt := range []string{"first", "second", "third"} {
		if got := newOutcome.messages[2*i].Content[0].Text; got != prompt {
			t.Fatalf("prompt %d = %q, want %q", i, got, prompt)
		}
	}
}

func TestSubagentCancellationReachesDescendantsAndPreservesOwnership(t *testing.T) {
	started := make(chan string, 4)
	a, parent := lifecycleApp(t, lifecycleModel(func(ctx context.Context, req core.Request, emit func(core.Delta)) (core.Response, error) {
		started <- req.SessionID
		<-ctx.Done()
		return core.Response{}, ctx.Err()
	}))
	childID, err := a.manager.spawn(parent.ID, "child", "child", "fresh")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	grandID, err := a.manager.spawn(childID, "grandchild", "grandchild", "fresh")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	other, err := a.store.Create(parent.CWD, "unrelated parent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.agentMessage(a.ctx, core.ToolContext{SessionID: other.ID}, core.JSON(map[string]string{"id": childID, "message": "unauthorized"})); err == nil {
		t.Fatal("another session sent a message to an unowned agent")
	}
	a.manager.mu.Lock()
	childDone, grandDone := a.manager.children[childID].done, a.manager.children[grandID].done
	a.manager.mu.Unlock()
	a.manager.cancelChildren(parent.ID)
	lifecycleDone(t, childDone)
	lifecycleDone(t, grandDone)
	a.manager.mu.Lock()
	for _, id := range []string{childID, grandID} {
		if a.manager.children[id].Status != "cancelled" {
			t.Errorf("%s status = %s", id, a.manager.children[id].Status)
		}
	}
	a.manager.mu.Unlock()
	cancelled, cancel := context.WithCancel(a.ctx)
	cancel()
	if _, err = a.manager.spawnContext(cancelled, parent.ID, "late work", "late", "fresh"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled tool created a subagent: %v", err)
	}
}

func TestSubagentForkInheritsProviderWorkspacePermissionsAndHistory(t *testing.T) {
	a, parent := lifecycleApp(t, lifecycleModel(func(ctx context.Context, req core.Request, emit func(core.Delta)) (core.Response, error) {
		return core.Response{Content: []core.Block{{Type: "text", Text: "finished"}}}, nil
	}))
	if _, err := a.store.Append(parent.ID, "prior", "turn/started", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	_, err := a.store.Append(parent.ID, "prior", "user/message", core.TextMessage("user", "prior task"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.store.Append(parent.ID, "prior", "assistant/message", core.TextMessage("assistant", "prior answer"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.store.Append(parent.ID, "prior", "turn/finished", map[string]any{"status": "completed"}); err != nil {
		t.Fatal(err)
	}
	id, err := a.manager.spawn(parent.ID, "fork task", "forked", "fork")
	if err != nil {
		t.Fatal(err)
	}
	a.manager.mu.Lock()
	done := a.manager.children[id].done
	a.manager.mu.Unlock()
	lifecycleDone(t, done)
	child, err := a.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if child.ParentID != parent.ID || child.CWD != parent.CWD || child.Permission != parent.Permission || child.Provider != parent.Provider || child.Model != parent.Model || child.Thinking != parent.Thinking {
		t.Fatalf("fork lost parent settings: %#v", child)
	}
	messages, err := a.store.Messages(id)
	if err != nil || len(messages) != 4 || messages[0].Content[0].Text != "prior task" {
		t.Fatalf("fork history = %#v; error=%v", messages, err)
	}
}

func TestSubagentFreshStartsWithoutParentHistory(t *testing.T) {
	var inherited atomic.Bool
	a, parent := lifecycleApp(t, lifecycleModel(func(ctx context.Context, req core.Request, emit func(core.Delta)) (core.Response, error) {
		if len(req.Messages) != 1 || req.Messages[0].Content[0].Text != "fresh task" {
			inherited.Store(true)
		}
		return core.Response{Content: []core.Block{{Type: "text", Text: "finished"}}}, nil
	}))
	_, err := a.store.Append(parent.ID, "prior", "user/message", core.TextMessage("user", "parent-only task"))
	if err != nil {
		t.Fatal(err)
	}
	id, err := a.manager.spawn(parent.ID, "fresh task", "fresh", "fresh")
	if err != nil {
		t.Fatal(err)
	}
	a.manager.mu.Lock()
	done := a.manager.children[id].done
	a.manager.mu.Unlock()
	lifecycleDone(t, done)
	if inherited.Load() {
		t.Fatal("fresh child inherited parent context")
	}
	if _, err = a.store.CreateChild(parent.CWD, "invalid parent", strings.Repeat("0", 32)); err == nil {
		t.Fatal("created a child with nonexistent parent")
	}
}

func TestSubagentAuditFailurePreventsModelDispatch(t *testing.T) {
	var called atomic.Int32
	a, parent := lifecycleApp(t, lifecycleModel(func(ctx context.Context, req core.Request, emit func(core.Delta)) (core.Response, error) {
		called.Add(1)
		return core.Response{Content: []core.Block{{Type: "text", Text: "unexpected"}}}, nil
	}))
	_, err := a.Execute(a.ctx, core.ToolContext{SessionID: parent.ID, CWD: parent.CWD, Emit: func(string, any) error { return errors.New("disk full") }}, "subagent", core.JSON(map[string]string{"description": "Work", "prompt": "work"}))
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("got %v", err)
	}
	if called.Load() != 0 {
		t.Fatal("model dispatched without a durable subagent start")
	}
	if len(a.manager.list(parent.ID)) != 0 {
		t.Fatal("failed start published a subagent")
	}
}

func TestPlanModeKeepsSandboxPermissionsIndependent(t *testing.T) {
	a, parent := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		return core.Response{}, nil
	}))
	if err := a.setPlan(parent.ID, true); err != nil {
		t.Fatal(err)
	}
	tc := core.ToolContext{SessionID: parent.ID, CWD: parent.CWD, Permission: "full-access", ApprovalPolicy: "never"}
	args := core.JSON(map[string]any{"file_path": "planned.txt", "content": "actual write"})
	if _, err := a.Execute(a.ctx, tc, "write", args); err != nil {
		t.Fatalf("plan guidance filtered a callable tool: %v", err)
	}
	tc.Permission = "read-only"
	if _, err := a.Execute(a.ctx, tc, "write", core.JSON(map[string]any{"file_path": "denied.txt", "content": "must fail"})); err == nil {
		t.Fatal("plan mode bypassed the independent read-only sandbox")
	}
}
