package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
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

func TestSCOR028OneShotSettlementReportsAutomaticFailure(t *testing.T) {
	want := errors.New("background result collection failed")
	var calls atomic.Int32
	a, parent := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		if calls.Add(1) == 1 {
			return core.Response{Content: []core.Block{{Type: "text", Text: "Waiting for background output."}}}, nil
		}
		return core.Response{}, want
	}))
	a.session = parent
	if err := a.turn("start work"); err != nil {
		t.Fatal(err)
	}
	a.deliverJobNotice(tools.JobNotice{Owner: parent.ID, Text: "background job finished", Summary: "job finished"})
	if err := a.settleDelegations(); !errors.Is(err, want) {
		t.Fatalf("automatic root failure was hidden from one-shot settlement: %v", err)
	}
}

func TestSCOR028NoOpWakeCannotEraseFailureAndRealRecoveryCan(t *testing.T) {
	want := errors.New("first automatic attempt failed")
	var calls atomic.Int32
	a, parent := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		if calls.Add(1) == 1 {
			return core.Response{}, want
		}
		return core.Response{Content: []core.Block{{Type: "text", Text: "Recovered and collected the output."}}}, nil
	}))
	// Pause assembly before inbox admission. Both messages then belong to the
	// failing activation; the competing wake merely observes an empty inbox.
	assembling, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	var assemblyEntered sync.Once
	t.Cleanup(func() { released.Do(func() { close(release) }) })
	a.engine.UpdateOptions(func(o *agent.Options) {
		o.SystemFor = func(core.Session) (string, error) {
			// Engine resolves SystemFor before admission and again for the
			// actual step; both visits participate in the same input gate.
			assemblyEntered.Do(func() { close(assembling) })
			<-release
			return "fixture system", nil
		}
	})
	if _, err := a.manager.deliver(a.ctx, parent.ID, core.TextMessage("user", "first owned result"), true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-assembling:
	case <-time.After(2 * time.Second):
		t.Fatal("automatic request was never assembled")
	}
	if _, err := a.manager.deliver(a.ctx, parent.ID, core.TextMessage("user", "second owned result"), true); err != nil {
		t.Fatal(err)
	}
	released.Do(func() { close(release) })
	settleOfficial(t, a, parent.ID)
	if calls.Load() != 1 {
		t.Fatalf("coalesced notices made %d calls, want one failed call and a no-op wake", calls.Load())
	}
	if err := a.manager.rootWakeError(parent.ID); !errors.Is(err, want) {
		t.Fatalf("the no-op wake erased the failure: %v", err)
	}
	// An explicit subsequent successful turn is a real recovery boundary.
	a.engine.UpdateOptions(func(o *agent.Options) { o.SystemFor = nil })
	if err := a.engine.Run(a.ctx, parent.ID, core.TextMessage("user", "retry collection")); err != nil {
		t.Fatal(err)
	}
	if err := a.manager.rootWakeError(parent.ID); err != nil {
		t.Fatalf("a successful recovery left a stale failure: %v", err)
	}
}

func TestSCOR028ChildAndUnrelatedRootFailuresDoNotFailSuccessfulParent(t *testing.T) {
	var parentID string
	var parentCalls atomic.Int32
	a, parent := lifecycleApp(t, lifecycleModel(func(_ context.Context, req core.Request, _ func(core.Delta)) (core.Response, error) {
		if req.SessionID != parentID {
			return core.Response{}, errors.New("delegated or unrelated work failed")
		}
		parentCalls.Add(1)
		return core.Response{Content: []core.Block{{Type: "text", Text: "I handled the child failure."}}}, nil
	}))
	parentID, a.session = parent.ID, parent
	childID, err := a.manager.spawnChild(a.ctx, parent.ID, "failing child", "fresh", core.TextMessage("user", "work"), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	settleOfficial(t, a, parent.ID)
	a.manager.mu.Lock()
	status := a.manager.children[childID].Status
	a.manager.mu.Unlock()
	if status != "failed" || parentCalls.Load() == 0 {
		t.Fatalf("missing actual failed child and successful parent collection: child=%s parent calls=%d", status, parentCalls.Load())
	}
	other, err := a.store.Create(parent.CWD, "unrelated root")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.manager.deliver(a.ctx, other.ID, core.TextMessage("user", "unrelated work"), true); err != nil {
		t.Fatal(err)
	}
	settleOfficial(t, a, other.ID)
	if err := a.manager.rootWakeError(other.ID); err == nil {
		t.Fatal("setup did not fail the unrelated root")
	}
	if err := a.settleDelegations(); err != nil {
		t.Fatalf("another root or a handled child failure contaminated the parent: %v", err)
	}
}

func TestSCOR028AutomaticCancellationIsAnOutcomeUntilRecovery(t *testing.T) {
	entered := make(chan struct{})
	var calls atomic.Int32
	a, parent := lifecycleApp(t, lifecycleModel(func(ctx context.Context, _ core.Request, _ func(core.Delta)) (core.Response, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-ctx.Done()
			return core.Response{}, ctx.Err()
		}
		return core.Response{Content: []core.Block{{Type: "text", Text: "Recovered."}}}, nil
	}))
	if _, err := a.manager.deliver(a.ctx, parent.ID, core.TextMessage("user", "collect background result"), true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("automatic turn did not start")
	}
	a.manager.pauseWakes(parent.ID)
	settleOfficial(t, a, parent.ID)
	if err := a.manager.rootWakeError(parent.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("automatic cancellation was hidden: %v", err)
	}
	a.manager.resumeWakes(parent.ID)
	if err := a.engine.Run(a.ctx, parent.ID, core.TextMessage("user", "resume")); err != nil {
		t.Fatal(err)
	}
	if err := a.manager.rootWakeError(parent.ID); err != nil {
		t.Fatalf("cancelled wake prevented later recovery: %v", err)
	}
}

func TestSCOR028MainRunReturnsNonzeroAfterAutomaticCollectionFailure(t *testing.T) {
	switch runtime.GOOS {
	case "darwin", "linux", "freebsd", "openbsd", "netbsd", "dragonfly":
	default:
		t.Skip("native bash process fixture requires a supported Unix host")
	}
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DSGO_API_KEY", "fixture-key")
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("DSGO_BASE_URL", "https://example.invalid/anthropic")
	gate := filepath.Join(cwd, "release")
	original := http.DefaultTransport
	defer func() { http.DefaultTransport = original }()
	var requests atomic.Int32
	http.DefaultTransport = roundTrip(func(*http.Request) (*http.Response, error) {
		switch requests.Add(1) {
		case 1:
			return jsonReply(`{"id":"bg-start","type":"message","content":[{"type":"tool_use","id":"bg","name":"bash","input":{"description":"Wait for the test release","command":"while [ ! -f release ]; do sleep 0.01; done; printf completed","run_in_background":true}}],"stop_reason":"tool_use"}`), nil
		case 2:
			return jsonReply(`{"id":"bg-wait","type":"message","content":[{"type":"text","text":"Waiting for the background result."}],"stop_reason":"end_turn"}`), nil
		default:
			response := jsonReply(`{"error":{"type":"invalid_request_error","message":"synthetic final collection failure"}}`)
			response.StatusCode = http.StatusBadRequest
			return response, nil
		}
	})
	var out, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- Main([]string{"--home", home, "--cwd", cwd, "--permission", "full-access", "--json", "run", "start background work"}, strings.NewReader(""), &out, &stderr)
	}()
	// Release the actual shell only after the first root turn durably completed,
	// so a failure cannot accidentally be attributed to the initial a.turn.
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	completed := false
	deadline := time.Now().Add(5 * time.Second)
	for !completed && time.Now().Before(deadline) {
		rows, _ := store.List(false)
		for _, row := range rows {
			events, _ := store.Events(row.ID)
			completed = completed || latestSuccessfulWakeTurn(events) > 0
		}
		if !completed {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if err := os.WriteFile(gate, []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if !completed || code == 0 || requests.Load() < 3 || !strings.Contains(stderr.String(), "INVALID_REQUEST") {
			t.Fatalf("root first completed=%t, exit=%d requests=%d stderr=%q", completed, code, requests.Load(), stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Main did not settle the failed automatic collection")
	}
}

func temporarilyUnreadableWakeLog(t *testing.T, a *App, id string) func() {
	t.Helper()
	path := filepath.Join(a.store.Root(), "sessions", id, "events.jsonl")
	held := path + ".review-held"
	if err := os.Rename(path, held); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		_ = os.Rename(held, path)
		t.Fatal(err)
	}
	var once sync.Once
	restore := func() {
		once.Do(func() {
			if err := os.Remove(path); err != nil {
				t.Error(err)
			}
			if err := os.Rename(held, path); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(restore)
	if _, err := a.store.Events(id); err == nil {
		restore()
		t.Fatal("test did not make the event log unreadable")
	}
	return restore
}

func TestSCOR028UnreadableFailureCanRecoverItsExactFinish(t *testing.T) {
	want := errors.New("automatic collection failed while metadata was unavailable")
	var calls atomic.Int32
	a, parent := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		if calls.Add(1) == 2 {
			return core.Response{}, want
		}
		return core.Response{Content: []core.Block{{Type: "text", Text: "Completed."}}}, nil
	}))
	if err := a.engine.Run(a.ctx, parent.ID, core.TextMessage("user", "first successful work")); err != nil {
		t.Fatal(err)
	}
	before, known := a.manager.wakeStartSequence(parent.ID)
	if !known {
		t.Fatal("missing wake start boundary")
	}
	if err := a.engine.Run(a.ctx, parent.ID, core.TextMessage("user", "failed collection")); !errors.Is(err, want) {
		t.Fatal(err)
	}
	restore := temporarilyUnreadableWakeLog(t, a, parent.ID)
	a.manager.recordWakeFailure(parent.ID, 1, before, known, want)
	restore()
	// Recovery can finish before the outcome observer reads successfully again.
	// Its exact failed finish in the known window permits this real success.
	if err := a.engine.Run(a.ctx, parent.ID, core.TextMessage("user", "retry collection")); err != nil {
		t.Fatal(err)
	}
	if err := a.manager.rootWakeError(parent.ID); err != nil {
		t.Fatalf("temporary outcome read failure became a permanent latch: %v", err)
	}
}

func TestSCOR028UnreadablePredispatchFailureRequiresLaterSuccess(t *testing.T) {
	a, parent := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		return core.Response{Content: []core.Block{{Type: "text", Text: "Completed."}}}, nil
	}))
	if err := a.engine.Run(a.ctx, parent.ID, core.TextMessage("user", "old successful work")); err != nil {
		t.Fatal(err)
	}
	before, known := a.manager.wakeStartSequence(parent.ID)
	parent.Archived = true
	if err := a.store.Update(parent); err != nil {
		t.Fatal(err)
	}
	want := a.engine.Continue(a.ctx, parent.ID)
	if want == nil {
		t.Fatal("archived root should fail before starting a turn")
	}
	restore := temporarilyUnreadableWakeLog(t, a, parent.ID)
	a.manager.recordWakeFailure(parent.ID, 1, before, known, want)
	restore()
	// There is no failed finish to identify. Do not credit a success which
	// happened before this failure; establish a conservative recovered tail.
	if err := a.manager.rootWakeError(parent.ID); !errors.Is(err, want) {
		t.Fatalf("old success incorrectly cleared an untracked failure: %v", err)
	}
	parent.Archived = false
	if err := a.store.Update(parent); err != nil {
		t.Fatal(err)
	}
	if err := a.engine.Run(a.ctx, parent.ID, core.TextMessage("user", "new successful recovery")); err != nil {
		t.Fatal(err)
	}
	if err := a.manager.rootWakeError(parent.ID); err != nil {
		t.Fatalf("conservative read-recovery anchor could never clear: %v", err)
	}
}

func TestSCOR028NewTrackedFailureReplacesRecoveredUnknownBoundary(t *testing.T) {
	first, second := errors.New("first automatic failure"), errors.New("second automatic failure")
	var calls atomic.Int32
	a, parent := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		switch calls.Add(1) {
		case 2:
			return core.Response{}, first
		case 3:
			return core.Response{}, second
		}
		return core.Response{Content: []core.Block{{Type: "text", Text: "Completed."}}}, nil
	}))
	if err := a.engine.Run(a.ctx, parent.ID, core.TextMessage("user", "baseline")); err != nil {
		t.Fatal(err)
	}
	before, known := a.manager.wakeStartSequence(parent.ID)
	if err := a.engine.Run(a.ctx, parent.ID, core.TextMessage("user", "fail first")); !errors.Is(err, first) {
		t.Fatal(err)
	}
	restore := temporarilyUnreadableWakeLog(t, a, parent.ID)
	a.manager.recordWakeFailure(parent.ID, 1, before, known, first)
	restore()
	before, known = a.manager.wakeStartSequence(parent.ID)
	if err := a.engine.Run(a.ctx, parent.ID, core.TextMessage("user", "fail second")); !errors.Is(err, second) {
		t.Fatal(err)
	}
	a.manager.recordWakeFailure(parent.ID, 2, before, known, second)
	if err := a.manager.rootWakeError(parent.ID); !errors.Is(err, second) {
		t.Fatalf("unknown prior boundary prevented newer tracked outcome: %v", err)
	}
	if err := a.engine.Run(a.ctx, parent.ID, core.TextMessage("user", "successful recovery")); err != nil {
		t.Fatal(err)
	}
	if err := a.manager.rootWakeError(parent.ID); err != nil {
		t.Fatalf("recovered failures still blocked success: %v", err)
	}
}

func TestSCOR028SettlementRechecksWakeRegisteredByCompletion(t *testing.T) {
	a, parent := lifecycleApp(t, lifecycleModel(nil))
	checked := false
	busy := a.manager.ownedWorkPending(parent.ID, func(owner string) bool {
		if owner != parent.ID {
			t.Fatalf("unexpected owner: %s", owner)
		}
		checked = true
		// The registry is now reporting its notice handed over, while the
		// corresponding managed wake is published but not yet Engine.Busy.
		a.manager.mu.Lock()
		a.manager.wakeRuns[1] = &parentWake{target: parent.ID, cancel: func() {}}
		a.manager.mu.Unlock()
		return false
	})
	if !checked || !busy || a.engine.Busy(parent.ID) {
		t.Fatalf("stale manager snapshot hid published completion wake: checked=%t busy=%t", checked, busy)
	}
	a.manager.mu.Lock()
	delete(a.manager.wakeRuns, 1)
	a.manager.mu.Unlock()
	if a.manager.ownedWorkPending(parent.ID, func(string) bool { return false }) {
		t.Fatal("settled root still reported owned work")
	}
}
