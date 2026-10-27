package cli

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"dsgo/internal/core"
	"dsgo/internal/schedule"
)

func TestCanonicalReminderToolsAndDelegatedGuard(t *testing.T) {
	a, parent := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		t.Fatal("reminder management must not call a model")
		return core.Response{}, nil
	}))
	if err := a.configureSchedules(); err != nil {
		t.Fatal(err)
	}
	tc := core.ToolContext{SessionID: parent.ID, CWD: parent.CWD, Permission: "read-only"}
	tc.Emit = func(kind string, data any) error { _, err := a.store.Append(parent.ID, "", kind, data); return err }
	result, err := a.Execute(a.ctx, tc, "schedule_create", json.RawMessage(`{"title":"检查项目","prompt":"review the project","after_seconds":36e2}`))
	if err != nil {
		t.Fatal(err)
	}
	var view schedule.View
	if err = json.Unmarshal([]byte(result.Content), &view); err != nil || view.ID == "" {
		t.Fatalf("invalid view %s %v", result.Content, err)
	}
	result, err = a.Execute(a.ctx, tc, "schedule_update", core.JSON(map[string]any{"id": view.ID, "title": "检查更新后的项目"}))
	if err != nil {
		t.Fatal(err)
	}
	var updated schedule.View
	_ = json.Unmarshal([]byte(result.Content), &updated)
	if updated.Title != "检查更新后的项目" || updated.ScheduledAt != view.ScheduledAt {
		t.Fatalf("edit re-timed reminder: %s", result.Content)
	}
	child, err := a.store.CreateChild(parent.CWD, "child", parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	tc.SessionID = child.ID
	if _, err = a.Execute(a.ctx, tc, "schedule_list", core.JSON(map[string]any{})); err == nil {
		t.Fatal("delegated child gained reminder control")
	}
	fork, err := a.store.Fork(parent.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	tc.SessionID = fork.ID
	if _, err = a.Execute(a.ctx, tc, "schedule_create", json.RawMessage(`{"title":"用户分叉","prompt":"fork reminder","after_seconds":3600}`)); err != nil {
		t.Fatalf("a user fork was mistaken for a delegated agent: %v", err)
	}
	tc.SessionID = parent.ID
	if _, err = a.Execute(a.ctx, tc, "schedule_delete", core.JSON(map[string]any{"id": view.ID})); err != nil {
		t.Fatal(err)
	}
	records, err := a.schedules.ListOfficial(a.ctx, parent.ID)
	if err != nil || len(records) != 0 {
		t.Fatalf("not deleted: %#v %v", records, err)
	}
}

func TestDaemonAcknowledgesDurableAdmissionBeforeModelCompletion(t *testing.T) {
	a, parent := lifecycleApp(t, lifecycleModel(func(ctx context.Context, r core.Request, emit func(core.Delta)) (core.Response, error) {
		<-ctx.Done()
		return core.Response{}, ctx.Err()
	}))
	a.unattended = true
	a.engine.Notify = a.notice
	if err := a.configureSchedules(); err != nil {
		t.Fatal(err)
	}
	seconds := int64(1)
	view, err := a.schedules.CreateOfficial(a.ctx, parent.ID, schedule.CreateRequest{Title: "Fixture", Prompt: "wait for cancellation", TimingInput: schedule.TimingInput{AfterSeconds: &seconds}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.cancel)
	done := make(chan error, 1)
	go func() { done <- a.daemon() }()
	deadline := time.Now().Add(5 * time.Second)
	var receipt string
	for time.Now().Before(deadline) {
		history, err := a.schedules.History(a.ctx, parent.ID, view.ID, 10, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(history.Records) > 0 {
			receipt = history.Records[0].MessageID
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if receipt == "" {
		t.Fatal("daemon did not commit a delivery receipt")
	}
	if !a.engine.Busy(parent.ID) {
		t.Fatal("receipt waited for model completion instead of durable admission")
	}
	events, err := a.store.Events(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	matched := false
	for _, ev := range events {
		if ev.Type == "user/message" && ev.ID == receipt {
			matched = true
		}
	}
	if !matched {
		t.Fatal("receipt is not the persistent user message identity")
	}
	a.cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("daemon exit: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon shutdown did not cancel its accepted model turn")
	}
}
