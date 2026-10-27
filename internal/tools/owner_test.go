package tools

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"dsgo/internal/core"
)

func TestSessionDisposalCancelsOnlyOwnedJobsWithoutWakingOwner(t *testing.T) {
	r := New()
	defer r.Close()
	var notices atomic.Int32
	r.SetJobNotifier(func(JobNotice) { notices.Add(1) })
	start := func(owner string) string {
		t.Helper()
		res, err := r.Execute(context.Background(), core.ToolContext{SessionID: owner, CWD: t.TempDir(), Permission: "full-access"}, "standard_bash", core.JSON(map[string]any{"description": "Hold a background command", "command": "sleep 10", "run_in_background": true}))
		if err != nil {
			t.Fatal(err)
		}
		var value struct {
			ID string `json:"jobId"`
		}
		if err = json.Unmarshal([]byte(res.Content), &value); err != nil || value.ID == "" {
			t.Fatalf("invalid job: %s %v", res.Content, err)
		}
		return value.ID
	}
	id, other := start("disposed"), start("other")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.CancelSessionJobs(ctx, "disposed"); err != nil {
		t.Fatal(err)
	}
	r.mu.RLock()
	one, two := r.jobs[id], r.jobs[other]
	r.mu.RUnlock()
	select {
	case <-one.done:
	default:
		t.Fatal("owned job did not settle")
	}
	select {
	case <-two.done:
		t.Fatal("unrelated job was canceled")
	default:
	}
	if notices.Load() != 0 {
		t.Fatal("owner disposal triggered a completion wake")
	}
}
