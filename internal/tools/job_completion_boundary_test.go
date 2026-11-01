//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"dsgo/internal/core"
)

func TestSCOR029PromotedJobCannotLoseCompletionNotice(t *testing.T) {
	r := New()
	defer r.Close()
	notices := make(chan JobNotice, 2)
	r.SetJobNotifier(func(n JobNotice) { notices <- n })
	tc := core.ToolContext{SessionID: "owner", CWD: t.TempDir(), Permission: "full-access"}
	type answer struct {
		res core.ToolResult
		err error
	}
	result := make(chan answer, 1)
	go func() {
		res, err := r.Execute(context.Background(), tc, "standard_bash", core.JSON(map[string]any{"description": "Produce a boundary result", "command": "sleep 0.03; printf completed", "timeoutMs": 100}))
		result <- answer{res, err}
	}()
	var j *job
	until := time.Now().Add(2 * time.Second)
	for j == nil && time.Now().Before(until) {
		r.mu.RLock()
		for _, entry := range r.jobs {
			j = entry
			break
		}
		r.mu.RUnlock()
		if j == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if j == nil {
		t.Fatal("job was never registered")
	}
	// Queue the real process settlement before the later timeout. Both paths
	// then contend for the unchanged production job lock on release.
	j.mu.Lock()
	time.Sleep(180 * time.Millisecond)
	j.mu.Unlock()
	var got answer
	select {
	case got = <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("bash caller stuck")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	var value struct{ Kind string }
	if err := json.Unmarshal([]byte(got.res.Content), &value); err != nil {
		t.Fatal(err)
	}
	<-j.noticed
	if value.Kind == "promoted" {
		select {
		case <-notices:
		default:
			t.Fatalf("actual job returned promoted yet settlement suppressed completion: %s", got.res.Content)
		}
	} else if value.Kind != "foreground" {
		t.Fatalf("unexpected result: %s", got.res.Content)
	}
	select {
	case n := <-notices:
		t.Fatalf("duplicate notice or foreground settlement also notified: %+v", n)
	default:
	}
}

func TestSCOR029CancelledWaitDeliversAlreadySettledSnapshot(t *testing.T) {
	// A live job_output wait and completion/cancellation can become ready
	// together. Since settlement suppresses the notice for a live waiter, the
	// waiter must still own that final value even if select observes cancel.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	j := &job{done: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- j.wait(ctx, time.Second) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		j.mu.Lock()
		if j.waiters == 1 {
			break
		}
		j.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("wait did not register")
		}
		time.Sleep(time.Millisecond)
	}
	// Hold retirement while both events become ready. Completion already owns
	// the waiter; cancellation cannot discard the only terminal delivery.
	cancel()
	close(j.done)
	j.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatalf("completed result lost at cancellation boundary: %v", err)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.waiters != 0 {
		t.Fatalf("waiter retirement leaked: %d", j.waiters)
	}
}

func TestSCOR029CancelledLiveWaitRetiresWithoutClaimingCompletion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	j := &job{done: make(chan struct{})}
	if err := j.wait(ctx, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("live wait ignored cancellation: %v", err)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.waiters != 0 {
		t.Fatalf("cancelled wait still suppresses future notice: %d", j.waiters)
	}
}
