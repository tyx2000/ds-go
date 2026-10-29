package cli

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"dsgo/internal/core"
	"dsgo/internal/state"
	"dsgo/internal/tools"
)

func TestJobQuietDeliveryLeavesIdleNoticePending(t *testing.T) {
	var calls atomic.Int32
	a, parent := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		calls.Add(1)
		return core.Response{Content: []core.Block{{Type: "text", Text: "done"}}}, nil
	}))
	a.cfg.Jobs = &state.JobSettings{CompletionDelivery: "quiet"}
	a.deliverJobNotice(tools.JobNotice{Owner: parent.ID, Text: "job finished", Summary: "test"})
	if calls.Load() != 0 {
		t.Fatal("quiet notice started a model turn")
	}
	pending, err := a.engine.PendingInbox(parent.ID)
	if err != nil || !pending {
		t.Fatalf("quiet notice was lost: %v %v", pending, err)
	}
}

func TestJobWakeCapResetsOnlyWhenHumanInputIsAdmitted(t *testing.T) {
	var calls atomic.Int32
	a, parent := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		calls.Add(1)
		return core.Response{Content: []core.Block{{Type: "text", Text: "done"}}}, nil
	}))
	a.engine.Notify = a.notice
	limit := 1
	a.cfg.Jobs = &state.JobSettings{MaxConsecutiveWakes: &limit}
	notice := tools.JobNotice{Owner: parent.ID, Text: "job finished", Summary: "test"}
	a.deliverJobNotice(notice)
	settleOfficial(t, a, parent.ID)
	if calls.Load() != 1 {
		t.Fatalf("first wake made %d requests", calls.Load())
	}
	a.deliverJobNotice(notice)
	settleOfficial(t, a, parent.ID)
	if calls.Load() != 1 {
		t.Fatal("job's own notice refilled the wake budget")
	}
	if pending, _ := a.engine.PendingInbox(parent.ID); !pending {
		t.Fatal("capped notice was discarded")
	}
	if err := a.engine.Run(a.ctx, parent.ID, core.TextMessage("user", "please continue")); err != nil {
		t.Fatal(err)
	}
	a.deliverJobNotice(notice)
	settleOfficial(t, a, parent.ID)
	if calls.Load() != 3 {
		t.Fatalf("human admission did not refill budget, calls=%d", calls.Load())
	}
}

func TestOwnedQuiescenceIgnoresUnrelatedWake(t *testing.T) {
	entered := make(chan struct{})
	otherID := ""
	a, parent := lifecycleApp(t, lifecycleModel(func(ctx context.Context, request core.Request, _ func(core.Delta)) (core.Response, error) {
		if request.SessionID == otherID {
			close(entered)
			<-ctx.Done()
			return core.Response{}, ctx.Err()
		}
		return core.Response{Content: []core.Block{{Type: "text", Text: "done"}}}, nil
	}))
	other, _ := a.store.Create(parent.CWD, "unrelated")
	otherID = other.ID
	if _, err := a.manager.deliver(a.ctx, other.ID, core.TextMessage("user", "unrelated ongoing work"), true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("unrelated owner was not running")
	}
	ctx, cancel := context.WithTimeout(a.ctx, 200*time.Millisecond)
	defer cancel()
	if err := a.manager.settle(ctx, parent.ID); err != nil {
		t.Fatalf("unrelated work blocked this owner: %v", err)
	}
}
