package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dsgo/internal/core"
	"dsgo/internal/workflow"
)

func TestSCOR006DelegatedForkExcludesInflightTurn(t *testing.T) {
	requests := make(chan core.Request, 1)
	a, parent := lifecycleApp(t, lifecycleModel(func(_ context.Context, request core.Request, _ func(core.Delta)) (core.Response, error) {
		requests <- request
		return core.Response{Content: []core.Block{{Type: "text", Text: "done"}}}, nil
	}))
	for _, record := range []struct {
		turn, kind string
		data       any
	}{
		{"completed", "turn/started", map[string]any{}},
		{"completed", "user/message", core.TextMessage("user", "completed prompt")},
		{"completed", "assistant/message", core.TextMessage("assistant", "completed answer")},
		{"completed", "turn/finished", map[string]any{"status": "completed"}},
		{"current", "turn/started", map[string]any{}},
		{"current", "user/message", core.TextMessage("user", "inflight-only prompt")},
		{"current", "assistant/message", core.Message{Role: "assistant", Content: []core.Block{{Type: "tool_use", ID: "open-call", Name: "subagent_fork", Input: json.RawMessage(`{}`)}}}},
	} {
		if _, err := a.store.Append(parent.ID, record.turn, record.kind, record.data); err != nil {
			t.Fatal(err)
		}
	}
	id, err := a.manager.spawn(parent.ID, "fork task", "fork", "fork")
	if err != nil {
		t.Fatal(err)
	}
	a.manager.mu.Lock()
	done := a.manager.children[id].done
	a.manager.mu.Unlock()
	lifecycleDone(t, done)
	request := <-requests
	text := string(core.JSON(request.Messages))
	if strings.Contains(text, "inflight-only") || strings.Contains(text, "open-call") || strings.Contains(text, "Previous process ended") {
		t.Fatalf("delegated fork inherited current unfinished turn: %s", text)
	}
	if !strings.Contains(text, "completed prompt") || !strings.Contains(text, "completed answer") {
		t.Fatalf("completed history lost: %s", text)
	}
}

func TestSCOR005BusyParentQueuesCompleteUserInputOnce(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	a, parent := lifecycleApp(t, lifecycleModel(func(ctx context.Context, request core.Request, _ func(core.Delta)) (core.Response, error) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return core.Response{}, ctx.Err()
			}
		}
		return core.Response{Content: []core.Block{{Type: "text", Text: "done"}}}, nil
	}))
	a.session = parent
	if _, err := a.manager.deliver(a.ctx, parent.ID, core.TextMessage("user", "automatic notice"), true); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := a.addDraftAttachment(parent.ID, core.Block{Type: "image", Source: map[string]any{"fixture": "durable-image"}}, 1); err != nil {
		t.Fatal(err)
	}
	if err := a.turn("typed while automatic turn runs"); err != nil {
		close(release)
		t.Fatal(err)
	}
	if a.hasDraftAttachments(parent.ID) {
		close(release)
		t.Fatal("durably queued attachment was not consumed")
	}
	events, err := a.store.Events(parent.ID)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	queued := 0
	for _, event := range events {
		if event.Type == "user/inbox" && strings.Contains(string(event.Data), "typed while automatic turn runs") {
			queued++
		}
	}
	if queued != 1 {
		close(release)
		t.Fatalf("input not durably queued exactly once: %d", queued)
	}
	close(release)
	settleOfficial(t, a, parent.ID)
	messages, err := a.store.Messages(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	admitted := 0
	for _, message := range messages {
		if len(message.Content) > 0 && message.Content[0].Text == "typed while automatic turn runs" {
			admitted++
			if len(message.Content) != 2 || message.Content[1].Type != "image" {
				t.Fatalf("queued media lost: %+v", message)
			}
		}
	}
	if admitted != 1 {
		t.Fatalf("queued input admission count=%d", admitted)
	}
}

func TestSCOR005RejectedAdmissionPreservesPendingAttachment(t *testing.T) {
	a, parent := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		t.Fatal("canceled input reached model")
		return core.Response{}, nil
	}))
	a.session = parent
	if err := a.addDraftAttachment(parent.ID, core.Block{Type: "image", Source: map[string]any{"fixture": "keep"}}, 1); err != nil {
		t.Fatal(err)
	}
	a.cancel()
	if err := a.turn("not admitted"); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if len(a.draftAttachments(parent.ID)) != 1 {
		t.Fatal("unadmitted attachment was discarded")
	}
}

func TestSCOR009SubtreeDrainWaitsForUnpublishedReservation(t *testing.T) {
	a, parent := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		return core.Response{Content: []core.Block{{Type: "text", Text: "done"}}}, nil
	}))
	entered, release := make(chan struct{}), make(chan struct{})
	startDone := make(chan error, 1)
	go func() {
		_, err := a.manager.spawnPrepared(a.ctx, parent.ID, "pending", "pending", "fresh", func(string) error { close(entered); <-release; return nil })
		startDone <- err
	}()
	<-entered
	drained := make(chan error, 1)
	go func() { drained <- a.manager.cancelAndDrain(context.Background(), parent.ID) }()
	// Wait until disposal has taken ownership, rather than relying on goroutine scheduling.
	deadline := time.Now().Add(time.Second)
	for {
		a.manager.mu.Lock()
		blocking := a.manager.draining[parent.ID] > 0
		a.manager.mu.Unlock()
		if blocking {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("drain did not start")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := a.manager.spawn(parent.ID, "late", "late", "fresh"); err == nil {
		close(release)
		t.Fatal("draining subtree admitted another start")
	}
	other, err := a.store.Create(parent.CWD, "unrelated")
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	id, err := a.manager.spawn(other.ID, "independent", "independent", "fresh")
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	a.manager.mu.Lock()
	otherDone := a.manager.children[id].done
	a.manager.mu.Unlock()
	lifecycleDone(t, otherDone)
	select {
	case err := <-drained:
		close(release)
		t.Fatalf("drain skipped unpublished start: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-startDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("unpublished start was not canceled: %v", err)
	}
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	a.manager.mu.Lock()
	defer a.manager.mu.Unlock()
	for _, child := range a.manager.children {
		if child.ParentID == parent.ID {
			t.Fatalf("late child was published: %+v", child)
		}
	}
}

func TestSCOR005WaitIdleReturnsAfterCurrentTurn(t *testing.T) {
	a, parent := lifecycleApp(t, lifecycleModel(func(ctx context.Context, _ core.Request, _ func(core.Delta)) (core.Response, error) {
		<-ctx.Done()
		return core.Response{}, ctx.Err()
	}))
	done := make(chan error, 1)
	go func() { done <- a.engine.Run(a.ctx, parent.ID, core.TextMessage("user", "hold")) }()
	deadline := time.Now().Add(time.Second)
	for !a.engine.Busy(parent.ID) {
		if time.Now().After(deadline) {
			t.Fatal("turn did not start")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(a.ctx, time.Millisecond)
	defer cancel()
	if err := a.engine.WaitIdle(ctx, parent.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitIdle ignored active owner: %v", err)
	}
	a.engine.Cancel(parent.ID)
	if err := a.engine.WaitIdle(a.ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if _, err := a.engine.Inbox(a.ctx, parent.ID, core.TextMessage("user", "later")); err != nil {
		t.Fatal(err)
	}
}

func TestSCOR006FirstTurnDelegatedForkHasEmptyPrefix(t *testing.T) {
	requests := make(chan core.Request, 1)
	a, parent := lifecycleApp(t, lifecycleModel(func(_ context.Context, request core.Request, _ func(core.Delta)) (core.Response, error) {
		requests <- request
		return core.Response{Content: []core.Block{{Type: "text", Text: "done"}}}, nil
	}))
	a.store.Append(parent.ID, "current", "turn/started", map[string]any{})
	a.store.Append(parent.ID, "current", "user/message", core.TextMessage("user", "inflight first prompt"))
	id, err := a.manager.spawn(parent.ID, "standalone fork task", "fork", "fork")
	if err != nil {
		t.Fatal(err)
	}
	a.manager.mu.Lock()
	done := a.manager.children[id].done
	a.manager.mu.Unlock()
	lifecycleDone(t, done)
	request := <-requests
	if len(request.Messages) != 1 || request.Messages[0].Content[0].Text != "standalone fork task" {
		t.Fatalf("first-turn fork should contain only its own task; request: %+v", request.Messages)
	}
}

func TestSCOR009WorkflowWaitsForDescendantCleanup(t *testing.T) {
	started, cancelling, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var a *App
	a, parent := lifecycleApp(t, lifecycleModel(func(ctx context.Context, request core.Request, _ func(core.Delta)) (core.Response, error) {
		if lifecyclePrompt(request) == "grandchild" {
			close(started)
			<-ctx.Done()
			close(cancelling)
			<-release
			return core.Response{}, ctx.Err()
		}
		if _, err := a.manager.spawnContext(ctx, request.SessionID, "grandchild", "grandchild", "fresh"); err != nil {
			return core.Response{}, err
		}
		<-started
		return core.Response{Content: []core.Block{{Type: "text", Text: "done"}}}, nil
	}))
	done := make(chan error, 1)
	go func() {
		_, err := a.workflowChild(a.ctx, parent.ID, workflow.ChildRequest{Seq: 1, Prompt: "member", Label: "worker"}, func(string) {})
		done <- err
	}()
	<-cancelling
	early := false
	select {
	case err := <-done:
		early = true
		t.Errorf("workflow settled before descendant cleanup: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	if early {
		return
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("workflow did not settle after cleanup")
	}
}
