package workflow

import (
	"context"
	"testing"
	"time"
)

func TestCancelOwnerWaitsItsChildrenAndLeavesUnrelatedRuns(t *testing.T) {
	entered := make(chan string, 2)
	e := testEngine(t, func(ctx context.Context, parent string, request ChildRequest, publish func(string)) (ChildResult, error) {
		publish(parent + "-child")
		entered <- parent
		<-ctx.Done()
		return ChildResult{StopReason: "cancelled"}, nil
	})
	start := func(owner string) *Run {
		t.Helper()
		run, err := e.Start(context.Background(), Request{ParentID: owner, CWD: t.TempDir(), Permission: "full-access", Script: `return await agent('wait');`, Meta: Meta{Name: "owned", Description: "owner cleanup"}})
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	owned, other := start("owned"), start("other")
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("child never entered")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := e.CancelOwner(ctx, "owned"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-owned.Done():
	default:
		t.Fatal("owner cancellation returned before settlement")
	}
	select {
	case <-other.Done():
		t.Fatal("unrelated workflow was cancelled")
	default:
	}
}
