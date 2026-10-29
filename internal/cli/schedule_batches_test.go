package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"dsgo/internal/core"
	"dsgo/internal/schedule"
)

func TestScheduledBusySessionDurablyQueuesBeforeReceipt(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	handled := make(chan struct{}, 4)
	a, parent := lifecycleApp(t, lifecycleModel(func(ctx context.Context, request core.Request, _ func(core.Delta)) (core.Response, error) {
		if lifecyclePrompt(request) == "occupy" {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return core.Response{}, ctx.Err()
			}
		} else {
			handled <- struct{}{}
		}
		return core.Response{Content: []core.Block{{Type: "text", Text: "done"}}}, nil
	}))
	a.unattended = true
	if err := a.configureSchedules(); err != nil {
		t.Fatal(err)
	}
	seconds := int64(1)
	view, err := a.schedules.CreateOfficial(a.ctx, parent.ID, schedule.CreateRequest{Title: "While busy", Prompt: "check later", TimingInput: schedule.TimingInput{AfterSeconds: &seconds}})
	if err != nil {
		t.Fatal(err)
	}
	foreground := make(chan error, 1)
	go func() { foreground <- a.engine.Run(a.ctx, parent.ID, core.TextMessage("user", "occupy")) }()
	<-started
	done := make(chan error, 1)
	go func() { done <- a.daemon() }()
	defer a.cancel()
	deadline := time.Now().Add(4 * time.Second)
	var messageID string
	for time.Now().Before(deadline) {
		history, err := a.schedules.History(a.ctx, parent.ID, view.ID, 10, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(history.Records) > 0 {
			messageID = history.Records[0].MessageID
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if messageID == "" {
		t.Fatal("busy session prevented durable receipt")
	}
	events, _ := a.store.Events(parent.ID)
	found := false
	for _, event := range events {
		if event.ID == messageID && event.Type == "user/inbox" {
			var entry struct {
				Message core.Message `json:"message"`
			}
			if err := json.Unmarshal(event.Data, &entry); err != nil {
				t.Fatal(err)
			}
			found = isScheduleMessage(entry.Message) && strings.HasPrefix(entry.Message.Content[0].Text, "[SCHEDULE REMINDER]\n")
		}
	}
	if !found {
		t.Fatal("receipt did not identify the sourced durable inbox message")
	}
	once.Do(func() { close(release) })
	if err := <-foreground; err != nil {
		t.Fatal(err)
	}
	select {
	case <-handled:
	case <-time.After(time.Second):
		t.Fatal("queued reminder did not wake after foreground turn")
	}
	a.cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("daemon did not stop")
	}
}

func TestScheduledRecoveryWakesUnadmittedInboxOnlyOnce(t *testing.T) {
	requests := make(chan struct{}, 4)
	a, parent := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		requests <- struct{}{}
		return core.Response{Content: []core.Block{{Type: "text", Text: "recovered"}}}, nil
	}))
	input := core.TextMessage("user", "[SCHEDULE REMINDER]\nfixture")
	input.Source = map[string]any{"kind": "schedule"}
	input.DeliveryID = "schedule-batch-recovery"
	if _, err := a.engine.Inbox(a.ctx, parent.ID, input); err != nil {
		t.Fatal(err)
	}
	if err := a.recoverScheduled(a.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requests:
	case <-time.After(time.Second):
		t.Fatal("pending reminder was not recovered")
	}
	a.background.Wait()
	if err := a.recoverScheduled(a.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requests:
		t.Fatal("recovery replayed an admitted finished reminder")
	case <-time.After(30 * time.Millisecond):
	}
}

func TestReminderToolsReturnPublicErrorsWithoutStorageDetails(t *testing.T) {
	a, parent, _ := officialApp(t)
	tc := core.ToolContext{SessionID: parent.ID}
	result, err := a.Execute(a.ctx, tc, "schedule_delete", core.JSON(map[string]any{"id": "  "}))
	if err != nil || !strings.Contains(result.Content, `"code":"invalid_rule"`) {
		t.Fatalf("invalid delete: %s %v", result.Content, err)
	}
	if err := a.schedules.SetOptions(schedule.Options{CheckSession: func(context.Context, string) error { return errors.New("fixture-secret-token /private/secret/path") }}); err != nil {
		t.Fatal(err)
	}
	result, err = a.Execute(a.ctx, tc, "schedule_create", core.JSON(map[string]any{"title": "valid", "prompt": "p", "after_seconds": 60}))
	if err != nil || !strings.Contains(result.Content, `"code":"internal_error"`) || strings.Contains(result.Content, "secret") || strings.Contains(result.Content, "/private") {
		t.Fatalf("storage failure leaked: %s %v", result.Content, err)
	}
}

func TestScheduledRecoveryDoesNotClaimOrdinaryDeliveryIDs(t *testing.T) {
	for _, test := range []struct{ id, text string }{{"input:user-draft", "Scheduled reminder (2026-10-27T09:00:00Z):\nordinary user request"}, {"inbox:message", "ordinary input"}, {"0123456789abcdef0123", "plain text with an opaque id"}} {
		t.Run(test.id, func(t *testing.T) {
			requests := make(chan struct{}, 1)
			a, parent := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
				requests <- struct{}{}
				return core.Response{}, nil
			}))
			input := core.TextMessage("user", test.text)
			input.DeliveryID = test.id
			if _, err := a.store.Append(parent.ID, "crashed", "turn/started", map[string]any{}); err != nil {
				t.Fatal(err)
			}
			if _, err := a.store.Append(parent.ID, "crashed", "user/message", input); err != nil {
				t.Fatal(err)
			}
			if err := a.recoverScheduled(a.ctx); err != nil {
				t.Fatal(err)
			}
			a.background.Wait()
			select {
			case <-requests:
				t.Fatal("schedule daemon started an ordinary unfinished input")
			default:
			}
		})
	}
}

func TestScheduledRecoveryRetainsTurnOriginAfterAgentSteering(t *testing.T) {
	requests := make(chan core.Request, 2)
	a, parent := lifecycleApp(t, lifecycleModel(func(_ context.Context, request core.Request, _ func(core.Delta)) (core.Response, error) {
		requests <- request
		return core.Response{Content: []core.Block{{Type: "text", Text: "recovered"}}}, nil
	}))
	scheduled := core.TextMessage("user", "[SCHEDULE REMINDER]\noriginal scheduled work")
	scheduled.Source = map[string]any{"kind": "schedule"}
	scheduled.DeliveryID = "schedule-batch-origin"
	steering := core.TextMessage("user", "agent progress after scheduled admission")
	steering.Source = map[string]any{"kind": "agent", "agentId": "child"}
	steering.DeliveryID = "inbox:agent-progress"
	for _, event := range []struct {
		kind string
		data any
	}{{"turn/started", map[string]any{}}, {"user/message", scheduled}, {"user/message", steering}} {
		if _, err := a.store.Append(parent.ID, "unfinished", event.kind, event.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.recoverScheduled(a.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-requests:
		if len(request.Messages) != 2 || request.Messages[0].DeliveryID != "schedule-batch-origin" || request.Messages[1].DeliveryID != "inbox:agent-progress" {
			t.Fatalf("recovery changed accepted history: %+v", request.Messages)
		}
	case <-time.After(time.Second):
		t.Fatal("later steering hid the scheduled turn origin")
	}
	a.background.Wait()
	if err := a.recoverScheduled(a.ctx); err != nil {
		t.Fatal(err)
	}
	a.background.Wait()
	select {
	case <-requests:
		t.Fatal("finished steered turn was recovered again")
	default:
	}
}
