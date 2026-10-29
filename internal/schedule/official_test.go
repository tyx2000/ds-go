package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pointer[T any](value T) *T { return &value }
func createOfficial(t *testing.T, s *Store, session, raw string) View {
	t.Helper()
	request, err := DecodeCreate(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	view, err := s.CreateOfficial(context.Background(), session, request)
	if err != nil {
		t.Fatal(err)
	}
	return view
}
func expectCode(t *testing.T, err error, code string) {
	t.Helper()
	var public *InputError
	if !errors.As(err, &public) || public.Code != code {
		t.Fatalf("got %v, expected public code %s", err, code)
	}
}
func stageDelivery(t *testing.T, s *Store, id, pending string, at time.Time) {
	t.Helper()
	err := s.locked(context.Background(), func() error {
		tasks, err := s.read()
		if err != nil {
			return err
		}
		for i := range tasks {
			if tasks[i].ID == id {
				tasks[i].Next = at
				if tasks[i].Kind == "once" {
					tasks[i].At = at
				}
				tasks[i].Pending = pending
				tasks[i].PendingAt = at
				value := tasks[i].Prompt
				tasks[i].PendingPrompt = &value
				return s.write(tasks)
			}
		}
		return errors.New("fixture task missing")
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOfficialCreateDecodingAndCanonicalSelectors(t *testing.T) {
	s, _ := Open(t.TempDir())
	view := createOfficial(t, s, "parent", `{"title":"  Review build  ","prompt":"  Check result  ","after_seconds":6e1}`)
	if view.Kind != "after" || view.AfterSeconds != 60 || view.Title != "Review build" || view.Prompt != "Check result" || view.DeliveryMode != "host" || view.State != "scheduled" {
		t.Fatalf("bad view: %#v", view)
	}
	weekly := createOfficial(t, s, "parent", `{"title":"Review","prompt":"Inspect","weekly":{"time":"09:30:00.5","time_zone":"Asia/Shanghai","weekdays":[7.0,1e0,3]}}`)
	if weekly.Time != "09:30:00.500" || fmt.Sprint(weekly.Weekdays) != "[1 3 7]" {
		t.Fatalf("noncanonical weekly: %#v", weekly)
	}
	long := createOfficial(t, s, "parent", `{"title":"Long interval","prompt":"Check","every_seconds":31557600000}`)
	if long.Kind != "every" || long.EverySeconds != 31557600000 {
		t.Fatal(long)
	}
	tasks, _ := s.List()
	var found Task
	for _, task := range tasks {
		if task.ID == long.ID {
			found = task
		}
	}
	if found.Next.Unix()-found.CreatedAt.Unix() != 31557600000 {
		t.Fatal("interval was limited by time.Duration")
	}
	for _, spec := range ToolSpecs() {
		if !json.Valid(spec.InputSchema) {
			t.Fatalf("invalid schema %s", spec.Name)
		}
	}
}

func TestOfficialCreateRejectsInvalidInputsWithoutPersisting(t *testing.T) {
	s, _ := Open(t.TempDir())
	tests := []struct{ raw, code string }{
		{`{"title":"x","prompt":"p","after_seconds":1,"every_seconds":60}`, "invalid_selector"},
		{`{"title":"x","prompt":"p","after_seconds":1,"unknown":true}`, "invalid_selector"},
		{`{"title":"x","prompt":"p","after_seconds":null}`, "invalid_rule"},
		{`{"title":"x","prompt":"p","every_seconds":59}`, "frequency_too_high"},
		{`{"title":"x","prompt":"p","every_seconds":60.5}`, "invalid_rule"},
		{`{"title":"x","prompt":"p","after_seconds":9007199254740992}`, "invalid_rule"},
		{`{"title":"x","prompt":"p","daily":{"time":"09:30","time_zone":"UTC"}}`, "invalid_rule"},
		{`{"title":"x","prompt":"p","daily":{"time":"09:30:00","time_zone":"Local"}}`, "invalid_time_zone"},
		{`{"title":"x","prompt":"p","weekly":{"time":"09:30:00","time_zone":"UTC","weekdays":[1,1]}}`, "invalid_rule"},
		{`{"title":"x","prompt":"p","at":{"date":"2027-03-14","time":"02:30:00","time_zone":"America/New_York"}}`, "invalid_rule"},
		{`{"title":"x","prompt":"p","at":"2020-01-01T00:00:00Z"}`, "not_future"},
		{`{"title":"x","prompt":"p","at":"2099-01-01T00:00:00-00:00"}`, "invalid_rule"},
	}
	for _, test := range tests {
		request, err := DecodeCreate(json.RawMessage(test.raw))
		if err == nil {
			_, err = s.CreateOfficial(context.Background(), "parent", request)
		}
		expectCode(t, err, test.code)
	}
	_, err := s.CreateOfficial(context.Background(), "parent", CreateRequest{Title: strings.Repeat("😀", 61), Prompt: "p", TimingInput: TimingInput{AfterSeconds: pointer(int64(60))}})
	expectCode(t, err, "invalid_prompt")
	all, _ := s.List()
	if len(all) != 0 {
		t.Fatalf("invalid input persisted %d tasks", len(all))
	}
}

func TestOfficialUpdateCASNoopReanchorAndImmutableHistory(t *testing.T) {
	s, _ := Open(t.TempDir())
	view := createOfficial(t, s, "parent", `{"title":"Review","prompt":"original","every_seconds":60}`)
	expected, _ := s.GetRecord(context.Background(), "parent", view.ID)
	result, err := s.UpdateOfficial(context.Background(), "parent", view.ID, expected, UpdatePatch{Timing: &TimingInput{EverySeconds: pointer(int64(60))}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated || result.Record.ScheduledAt != expected.ScheduledAt {
		t.Fatal("equivalent timing reanchored")
	}
	result, err = s.UpdateOfficial(context.Background(), "parent", view.ID, expected, UpdatePatch{Title: pointer("New title")})
	if err != nil || !result.Updated {
		t.Fatal(result, err)
	}
	stale, err := s.UpdateOfficial(context.Background(), "parent", view.ID, expected, UpdatePatch{Prompt: pointer("overwrite")})
	if err != nil || stale.Code != "schedule_conflict" {
		t.Fatal(stale, err)
	}
	invalid := *result.Record
	invalid.Title = ""
	_, err = s.UpdateOfficial(context.Background(), "parent", view.ID, invalid, UpdatePatch{Prompt: pointer("invalid")})
	expectCode(t, err, "invalid_rule")
	stageDelivery(t, s, view.ID, "delivery-1", time.Now().Add(-time.Minute).Truncate(time.Millisecond))
	current, _ := s.GetRecord(context.Background(), "parent", view.ID)
	_, err = s.UpdateOfficial(context.Background(), "parent", view.ID, current, UpdatePatch{Prompt: pointer("changed after admission")})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Acknowledge(context.Background(), "parent", view.ID, "delivery-1", "message-1"); err != nil {
		t.Fatal(err)
	}
	if err = s.Acknowledge(context.Background(), "parent", view.ID, "delivery-1", "message-1"); err != nil {
		t.Fatal("receipt not idempotent", err)
	}
	history, err := s.History(context.Background(), "parent", view.ID, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Records) != 1 || history.Records[0].Prompt == nil || *history.Records[0].Prompt != "original" {
		t.Fatalf("sent snapshot changed: %#v", history)
	}
	current, _ = s.GetRecord(context.Background(), "parent", view.ID)
	after := json.RawMessage(`"2099-01-01T00:00:00Z"`)
	changed, err := s.UpdateOfficial(context.Background(), "parent", view.ID, current, UpdatePatch{Timing: &TimingInput{At: after}})
	if err != nil {
		t.Fatal(err)
	}
	if changed.Record.Kind != "at" || changed.Record.ID != view.ID || changed.Record.Title != "New title" {
		t.Fatal(changed)
	}
	history, _ = s.History(context.Background(), "parent", view.ID, 100, "")
	if len(history.Records) != 1 {
		t.Fatal("timing update replaced history")
	}
}

func TestAcknowledgmentPreventsLegacySettlementDoubleAdvance(t *testing.T) {
	s, _ := Open(t.TempDir())
	view := createOfficial(t, s, "parent", `{"title":"Check","prompt":"original","after_seconds":60}`)
	stageDelivery(t, s, view.ID, "already-claimed", time.Now().Add(-time.Second).Truncate(time.Millisecond))
	calls := 0
	if err := s.tick(context.Background(), time.Now(), func(ctx context.Context, task Task) error {
		calls++
		return s.Acknowledge(ctx, task.SessionID, task.ID, task.Pending, "persisted-user-event")
	}); err != nil {
		t.Fatal(err)
	}
	tasks, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(tasks) != 1 || tasks[0].Enabled || tasks[0].Pending != "" {
		t.Fatal(tasks)
	}
	history, _ := s.History(context.Background(), "parent", view.ID, 10, "")
	if len(history.Records) != 1 || history.Records[0].MessageID != "persisted-user-event" {
		t.Fatal(history)
	}
	active, _ := s.ListOfficial(context.Background(), "parent")
	if len(active) != 0 {
		t.Fatal("ended one-shot remained active")
	}
}

func TestOfficialHistoryRetentionPaginationAndPhysicalDeletion(t *testing.T) {
	s, _ := Open(t.TempDir())
	if err := s.SetOptions(Options{DeliveryHistoryDays: 30, DeliveryHistoryRecords: 2}); err != nil {
		t.Fatal(err)
	}
	view := createOfficial(t, s, "parent", `{"title":"Check","prompt":"Check build","every_seconds":60}`)
	for i := 1; i <= 4; i++ {
		pending := fmt.Sprint("p", i)
		stageDelivery(t, s, view.ID, pending, time.Now().Add(-time.Minute).Truncate(time.Millisecond))
		if err := s.Acknowledge(context.Background(), "parent", view.ID, pending, fmt.Sprint("m", i)); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.History(context.Background(), "parent", view.ID, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 || page.Records[0].MessageID != "m4" || page.NextBefore != "m4" || !page.EarlierRecordsPruned || !page.EarlierRecordsUnavailable {
		t.Fatal(page)
	}
	older, _ := s.History(context.Background(), "parent", view.ID, 1, page.NextBefore)
	if len(older.Records) != 1 || older.Records[0].MessageID != "m3" || older.NextBefore != "" {
		t.Fatal(older)
	}
	missing, _ := s.History(context.Background(), "parent", view.ID, 1, "unknown")
	if missing.Code != "delivery_cursor_not_found" {
		t.Fatal(missing)
	}
	wrong, _ := s.DeleteOfficial(context.Background(), "other", view.ID)
	if wrong.Deleted || wrong.Code != "schedule_not_found" {
		t.Fatal(wrong)
	}
	deleted, err := s.DeleteOfficial(context.Background(), "parent", view.ID)
	if err != nil || !deleted.Deleted {
		t.Fatal(deleted, err)
	}
	history, _ := s.History(context.Background(), "parent", view.ID, 1, "")
	if history.Code != "schedule_not_found" {
		t.Fatal(history)
	}
	catalog, _ := s.Catalog(context.Background())
	if len(catalog) != 0 {
		t.Fatal("delete retained task")
	}
	_, err = s.History(context.Background(), "parent", view.ID, 0, "")
	expectCode(t, err, "invalid_rule")
}

func TestReceiptHistoryUsesAppendOrderAndAcknowledgmentWindow(t *testing.T) {
	task := Task{Prompt: "snapshot", HistoryKnown: true}
	options := defaultOptions()
	now := time.Now().UTC().Truncate(time.Millisecond)
	appendReceipt(&task, Receipt{ScheduledAt: instant(now.AddDate(0, 0, -40)), DeliveredAt: instant(now.AddDate(0, 0, -40)), MessageID: "old"}, options)
	appendReceipt(&task, Receipt{ScheduledAt: instant(now), DeliveredAt: instant(now), MessageID: "new"}, options)
	if len(task.DeliveryHistory) != 1 || task.DeliveryHistory[0].MessageID != "new" || !task.EarlierRecordsPruned {
		t.Fatal(task.DeliveryHistory)
	}
	appendReceipt(&task, Receipt{ScheduledAt: instant(now.Add(-time.Hour)), DeliveredAt: instant(now.Add(-time.Hour)), MessageID: "clock-backward"}, options)
	if task.DeliveryHistory[len(task.DeliveryHistory)-1].MessageID != "clock-backward" || task.LastDelivery.MessageID != "clock-backward" {
		t.Fatal("history sorted by wall time instead of append order")
	}
}

func TestOfficialDelegatedAdmissionAndArchiveStop(t *testing.T) {
	s, _ := Open(t.TempDir())
	child := createOfficial(t, s, "child", `{"title":"Old child task","prompt":"p","every_seconds":60}`)
	if err := s.SetOptions(Options{CheckSession: func(ctx context.Context, id string) error {
		if id == "child" {
			return &InputError{Code: "subagent_session", Message: "A delegated subagent cannot use reminders."}
		}
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	_, err := s.CreateOfficial(context.Background(), "child", CreateRequest{Title: "New", Prompt: "p", TimingInput: TimingInput{AfterSeconds: pointer(int64(60))}})
	expectCode(t, err, "subagent_session")
	expected, _ := s.GetRecord(context.Background(), "child", child.ID)
	refused, err := s.UpdateOfficial(context.Background(), "child", child.ID, expected, UpdatePatch{Title: pointer("Changed")})
	if err != nil || refused.Updated || refused.Code != "subagent_session" {
		t.Fatalf("delegated update: %+v %v", refused, err)
	}
	list, err := s.ListOfficial(context.Background(), "child")
	if err != nil || len(list) != 1 {
		t.Fatal("historical child task became unmanageable")
	}
	if result, err := s.DeleteOfficial(context.Background(), "child", child.ID); err != nil || !result.Deleted {
		t.Fatal(result, err)
	}
	ended := createOfficial(t, s, "parent", `{"title":"Delivered","prompt":"p","after_seconds":60}`)
	stageDelivery(t, s, ended.ID, "ended", time.Now().Add(-time.Second).Truncate(time.Millisecond))
	if err = s.Acknowledge(context.Background(), "parent", ended.ID, "ended", "ended-message"); err != nil {
		t.Fatal(err)
	}
	createOfficial(t, s, "parent", `{"title":"Active","prompt":"p","after_seconds":60}`)
	createOfficial(t, s, "other", `{"title":"Other","prompt":"p","after_seconds":60}`)
	if err = s.StopSession(context.Background(), "parent"); err != nil {
		t.Fatal(err)
	}
	catalog, _ := s.Catalog(context.Background())
	if len(catalog) != 2 {
		t.Fatalf("archive stop should keep inactive and other-session rows: %#v", catalog)
	}
	active, _ := s.ListOfficial(context.Background(), "parent")
	if len(active) != 0 {
		t.Fatal(active)
	}
}

func TestLegacyRowsAreReadableAndMigrateOnlyWithExplicitTitle(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(root)
	legacy, err := s.Create(Task{SessionID: "parent", Prompt: "  old instruction  ", Kind: "daily", Clock: "09:30", Timezone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(root, "schedules", "tasks.json"))
	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(root, "schedules", "tasks.json"))
	if string(before) != string(after) {
		t.Fatal("read-time migration rewrote old rows")
	}
	expected, _ := reopened.GetRecord(context.Background(), "parent", legacy.ID)
	_, err = reopened.UpdateOfficial(context.Background(), "parent", legacy.ID, expected, UpdatePatch{Prompt: pointer("new instruction")})
	expectCode(t, err, "invalid_prompt")
	updated, err := reopened.UpdateOfficial(context.Background(), "parent", legacy.ID, expected, UpdatePatch{Title: pointer("Explicit name")})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Record.Title != "Explicit name" || updated.Record.Time != "09:30:00.000" || updated.Record.Prompt != "old instruction" {
		t.Fatal(updated)
	}
	if _, err = Open(root); err != nil {
		t.Fatal(err)
	}
}

func TestOfficialCronCanonicalizationAndCenturyLeapSearch(t *testing.T) {
	tests := map[string]string{"0,0,15,30,45 9-17/1 * * 7": "0-45/15 9-17 * * 0", "*/2,1 * */2 * 1": "*/2,1 * */2 * 1", "0 0 1-31 * *": "0 0 1-31 * *", "*/100 * * * *": "*/60 * * * *"}
	for input, want := range tests {
		got, err := CanonicalCron(input)
		if err != nil || got != want {
			t.Fatalf("%q => %q (%v), want %q", input, got, err, want)
		}
		again, err := CanonicalCron(got)
		if err != nil || again != got {
			t.Fatalf("canonical form not idempotent: %s", got)
		}
	}
	if _, err := CanonicalCron("1/2 * * * *"); err == nil {
		t.Fatal("unsupported value/step accepted")
	}
	next, err := Next(Task{Kind: "cron", Cron: "0 0 29 2 *", Timezone: "UTC"}, stamp("2096-03-01T00:00:00Z"))
	if err != nil || !next.Equal(stamp("2104-02-29T00:00:00Z")) {
		t.Fatalf("400-year calendar search failed: %s %v", next, err)
	}
}

func TestDecodeOfficialUpdateStrictKeysAndUnions(t *testing.T) {
	id, patch, err := DecodeUpdate(json.RawMessage(`{"id":"id","title":"New","every_seconds":6e1}`))
	if err != nil || id != "id" || patch.Title == nil || *patch.Timing.EverySeconds != 60 {
		t.Fatal(id, patch, err)
	}
	for _, raw := range []string{`{"id":"id"}`, `{"id":"id","after_seconds":60}`, `{"id":"id","unknown":true}`, `{"id":"id","title":null}`, `{"id":"id","daily":{"time":"09:30:00","time_zone":"UTC","unknown":true}}`} {
		if _, _, err := DecodeUpdate(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}

func TestTimingEditInvalidatesPendingAdmissionWithoutCorruptingStore(t *testing.T) {
	s, _ := Open(t.TempDir())
	view := createOfficial(t, s, "parent", `{"title":"Check","prompt":"old","every_seconds":60}`)
	stageDelivery(t, s, view.ID, "old-attempt", time.Now().Add(-time.Second).Truncate(time.Millisecond))
	expected, _ := s.GetRecord(context.Background(), "parent", view.ID)
	result, err := s.UpdateOfficial(context.Background(), "parent", view.ID, expected, UpdatePatch{Timing: &TimingInput{EverySeconds: pointer(int64(120))}})
	if err != nil || !result.Updated {
		t.Fatal(result, err)
	}
	tasks, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if tasks[0].Pending != "" || tasks[0].PendingPrompt != nil || tasks[0].EverySeconds != 120 {
		t.Fatal(tasks[0])
	}
	if err = s.Acknowledge(context.Background(), "parent", view.ID, "old-attempt", "stale-message"); err == nil {
		t.Fatal("stale admission changed updated timing")
	}
}

func TestPendingSnapshotMatchesDispatchedContentAfterInstructionEdit(t *testing.T) {
	s, _ := Open(t.TempDir())
	view := createOfficial(t, s, "parent", `{"title":"Check","prompt":"old snapshot","every_seconds":60}`)
	stageDelivery(t, s, view.ID, "pending", time.Now().Add(-time.Second).Truncate(time.Millisecond))
	expected, _ := s.GetRecord(context.Background(), "parent", view.ID)
	if _, err := s.UpdateOfficial(context.Background(), "parent", view.ID, expected, UpdatePatch{Prompt: pointer("new future instruction")}); err != nil {
		t.Fatal(err)
	}
	if err := s.tick(context.Background(), time.Now(), func(ctx context.Context, task Task) error {
		if task.Prompt != "old snapshot" {
			t.Errorf("pending instruction changed: %s", task.Prompt)
		}
		return s.Acknowledge(ctx, task.SessionID, task.ID, task.Pending, "accepted-message")
	}); err != nil {
		t.Fatal(err)
	}
	record, _ := s.GetRecord(context.Background(), "parent", view.ID)
	if record.Prompt != "new future instruction" {
		t.Fatal("future rule lost edited instruction")
	}
	history, _ := s.History(context.Background(), "parent", view.ID, 10, "")
	if len(history.Records) != 1 || *history.Records[0].Prompt != "old snapshot" {
		t.Fatal(history)
	}
}
