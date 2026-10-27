package schedule

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func stamp(s string) time.Time {
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return v
}
func TestCalendarSchedules(t *testing.T) {
	tests := []struct {
		name        string
		task        Task
		after, want string
	}{
		{"daily", Task{Kind: "daily", Clock: "09:30", Timezone: "Asia/Shanghai"}, "2026-10-26T02:00:00Z", "2026-10-27T01:30:00Z"},
		{"weekly", Task{Kind: "weekly", Clock: "08:00", Weekday: 1, Timezone: "UTC"}, "2026-10-26T09:00:00Z", "2026-11-02T08:00:00Z"},
		{"cron dom dow OR", Task{Kind: "cron", Cron: "0 9 1 * 1", Timezone: "UTC"}, "2026-10-26T10:00:00Z", "2026-11-01T09:00:00Z"},
		{"cron steps", Task{Kind: "cron", Cron: "*/15 * * * *", Timezone: "UTC"}, "2026-10-26T09:01:20Z", "2026-10-26T09:15:00Z"},
		{"spring gap skipped", Task{Kind: "daily", Clock: "02:30", Timezone: "America/New_York"}, "2026-03-07T08:00:00Z", "2026-03-09T06:30:00Z"},
		{"daily overlap only earlier", Task{Kind: "daily", Clock: "01:30", Timezone: "America/New_York"}, "2026-11-01T05:31:00Z", "2026-11-02T06:30:00Z"},
		{"cron overlap only earlier", Task{Kind: "cron", Cron: "30 1 * * *", Timezone: "America/New_York"}, "2026-11-01T05:31:00Z", "2026-11-02T06:30:00Z"},
		{"half hour overlap", Task{Kind: "daily", Clock: "01:45", Timezone: "Australia/Lord_Howe"}, "2026-04-04T14:46:00Z", "2026-04-05T15:15:00Z"},
		{"cron star step keeps AND", Task{Kind: "cron", Cron: "0 0 */2 * 1", Timezone: "UTC"}, "2026-10-25T00:00:00Z", "2026-11-09T00:00:00Z"},
		{"fractional local clock", Task{Kind: "daily", Clock: "09:30:15.125", Timezone: "Asia/Shanghai"}, "2026-10-26T02:00:00Z", "2026-10-27T01:30:15.125Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Next(tt.task, stamp(tt.after))
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(stamp(tt.want)) {
				t.Fatalf("got %s want %s", got, tt.want)
			}
		})
	}
}
func TestPendingDeliverySurvivesRestartAndRetriesSameID(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.Create(Task{SessionID: "session", Prompt: "hello", Kind: "once", At: time.Now().Add(time.Minute), Timezone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(2 * time.Minute)
	pending := ""
	if err = s.tick(context.Background(), now, func(ctx context.Context, task Task) error {
		pending = task.Pending
		return errors.New("temporarily unavailable")
	}); err != nil {
		t.Fatal(err)
	}
	if pending == "" {
		t.Fatal("no persistent delivery id")
	}
	reopened, _ := Open(root)
	if err = reopened.tick(context.Background(), now.Add(time.Minute), func(ctx context.Context, task Task) error {
		if task.Pending != pending {
			t.Fatal("delivery ID changed across retry")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	all, _ := reopened.List()
	if len(all) != 1 || all[0].Enabled || all[0].Pending != "" || all[0].ID != task.ID {
		t.Fatalf("not settled: %#v", all)
	}
}
func TestMissedIntervalsDoNotReplayEveryOccurrence(t *testing.T) {
	s, _ := Open(t.TempDir())
	task, err := s.Create(Task{SessionID: "session", Prompt: "hello", Kind: "interval", Every: "1h", Timezone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	now := task.CreatedAt.Add(1000*time.Hour + time.Minute)
	calls := 0
	if err = s.tick(context.Background(), now, func(_ context.Context, delivered Task) error {
		calls++
		if !delivered.PendingAt.Equal(task.CreatedAt.Add(1000 * time.Hour)) {
			t.Errorf("did not select latest missed occurrence: %s", delivered.PendingAt)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = s.tick(context.Background(), now, func(context.Context, Task) error { calls++; return nil }); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("replayed %d occurrences", calls)
	}
	all, _ := s.List()
	if !all[0].Next.Equal(task.CreatedAt.Add(1001 * time.Hour)) {
		t.Fatalf("rate drift: %s", all[0].Next)
	}
}

func await(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for scheduler")
	}
}
func awaitError(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for scheduler")
		return nil
	}
}

func TestConcurrentSessionsProgressAndSameSessionIsSerial(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(time.Minute)
	a, err := s.Create(Task{SessionID: "a", Prompt: "first", Kind: "once", At: base, Timezone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	a2, err := s.Create(Task{SessionID: "a", Prompt: "second", Kind: "once", At: base.Add(time.Second), Timezone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Create(Task{SessionID: "b", Prompt: "independent", Kind: "once", At: base, Timezone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	aStarted, bFinished, releaseA := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-releaseA:
		default:
			close(releaseA)
		}
	}()
	var mu sync.Mutex
	active := map[string]bool{}
	order := []string{}
	done := make(chan error, 1)
	go func() {
		done <- s.tick(context.Background(), base.Add(time.Minute), func(ctx context.Context, task Task) error {
			mu.Lock()
			if active[task.SessionID] {
				t.Errorf("concurrent callbacks for session %s", task.SessionID)
			}
			active[task.SessionID] = true
			if task.SessionID == "a" {
				order = append(order, task.ID)
			}
			mu.Unlock()
			if task.ID == a.ID {
				close(aStarted)
				<-releaseA
			}
			if task.SessionID == "b" {
				close(bFinished)
			}
			mu.Lock()
			active[task.SessionID] = false
			mu.Unlock()
			return nil
		})
	}()
	await(t, aStarted)
	await(t, bFinished)
	close(releaseA)
	if err := awaitError(t, done); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != a.ID || order[1] != a2.ID {
		t.Fatalf("session order: %v", order)
	}
}

func TestCancelBeforeQueuedAdmissionPreventsCallback(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(root)
	other, _ := Open(root)
	base := time.Now().Add(time.Minute)
	first, _ := s.Create(Task{SessionID: "a", Prompt: "first", Kind: "once", At: base, Timezone: "UTC"})
	second, _ := s.Create(Task{SessionID: "a", Prompt: "second", Kind: "once", At: base.Add(time.Second), Timezone: "UTC"})
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	var secondCalled atomic.Bool
	go func() {
		done <- s.tick(context.Background(), base.Add(time.Minute), func(ctx context.Context, task Task) error {
			if task.ID == first.ID {
				close(started)
				<-release
			}
			if task.ID == second.ID {
				secondCalled.Store(true)
			}
			return nil
		})
	}()
	await(t, started)
	if err := other.Cancel(second.ID); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	if err := awaitError(t, done); err != nil {
		t.Fatal(err)
	}
	if secondCalled.Load() {
		t.Fatal("cancelled task was dispatched")
	}
}

func TestCrossStoreCancellationCancelsInFlightAdmission(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(root)
	other, _ := Open(root)
	base := time.Now().Add(time.Minute)
	task, _ := s.Create(Task{SessionID: "a", Prompt: "blocked restore", Kind: "once", At: base, Timezone: "UTC"})
	started, cancelled := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.tick(context.Background(), base.Add(time.Minute), func(ctx context.Context, _ Task) error {
			close(started)
			<-ctx.Done()
			close(cancelled)
			return ctx.Err()
		})
	}()
	await(t, started)
	if err := other.Cancel(task.ID); err != nil {
		t.Fatal(err)
	}
	await(t, cancelled)
	if err := awaitError(t, done); err != nil {
		t.Fatal(err)
	}
	all, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Enabled || all[0].Pending != "" {
		t.Fatalf("cancel state overwritten: %#v", all)
	}
}

func TestOverlappingTicksAndStoresCannotDuplicateAdmission(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(root)
	other, _ := Open(root)
	base := time.Now().Add(time.Minute)
	_, _ = s.Create(Task{SessionID: "a", Prompt: "hello", Kind: "once", At: base, Timezone: "UTC"})
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	done := make(chan error, 1)
	callback := func(ctx context.Context, _ Task) error { calls.Add(1); close(started); <-release; return nil }
	go func() { done <- s.tick(context.Background(), base.Add(time.Minute), callback) }()
	await(t, started)
	if err := other.tick(context.Background(), base.Add(time.Minute), func(context.Context, Task) error { calls.Add(1); return nil }); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	if err := awaitError(t, done); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("callback invoked %d times", calls.Load())
	}
}

func TestDaemonRecoveryRunsOnlyUnderUniqueLockAndCanRestart(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(root)
	other, _ := Open(root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.RunWithStart(ctx, func(context.Context) error { close(started); return nil }, func(context.Context, Task) error { return nil })
	}()
	await(t, started)
	var forbidden atomic.Bool
	err := other.RunWithStart(context.Background(), func(context.Context) error { forbidden.Store(true); return nil }, func(context.Context, Task) error { return nil })
	if err == nil || forbidden.Load() {
		t.Fatal("second daemon entered recovery")
	}
	cancel()
	if err := awaitError(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	called := false
	err = other.RunWithStart(ctx2, func(context.Context) error { called = true; cancel2(); return nil }, func(context.Context, Task) error { return nil })
	if !called || !errors.Is(err, context.Canceled) {
		t.Fatalf("could not reopen daemon: called=%v err=%v", called, err)
	}
}

func TestLatestMissedWallClockOccurrenceUsesEarlierOverlap(t *testing.T) {
	for _, kind := range []string{"daily", "cron"} {
		t.Run(kind, func(t *testing.T) {
			task := Task{Kind: kind, Clock: "01:30", Cron: "30 1 * * *", Timezone: "America/New_York", Next: stamp("2026-10-31T05:30:00Z")}
			got, err := latestOccurrence(task, stamp("2026-11-01T06:31:00Z"))
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(stamp("2026-11-01T05:30:00Z")) {
				t.Fatal(got)
			}
		})
	}
}

func TestInvalidStoreRejectsOpenWithoutRewriting(t *testing.T) {
	root := t.TempDir()
	folder := filepath.Join(root, "schedules")
	if err := os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(folder, "tasks.json")
	original := []byte("null\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); err == nil {
		t.Fatal("corrupt store was accepted")
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(original) {
		t.Fatal("failed recovery rewrote store")
	}
}

func TestRunningDaemonAdmitsNewDueSessionWhileAnotherRestoreWaits(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstStarted, secondStarted := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	_, err = s.Create(Task{SessionID: "blocked", Prompt: "first", Kind: "once", At: time.Now().Add(20 * time.Millisecond), Timezone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		done <- s.Run(ctx, func(ctx context.Context, task Task) error {
			if task.SessionID == "blocked" {
				close(firstStarted)
				<-ctx.Done()
				return ctx.Err()
			}
			close(secondStarted)
			return nil
		})
	}()
	await(t, firstStarted)
	_, err = s.Create(Task{SessionID: "new", Prompt: "second", Kind: "once", At: time.Now().Add(20 * time.Millisecond), Timezone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	await(t, secondStarted)
	cancel()
	if err := awaitError(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCronLargeStepCannotOverflowAndInvalidNumericFormsReject(t *testing.T) {
	cron, err := parseCron("1-59/9223372036854775807 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	if len(cron.fields[0]) != 1 || !cron.fields[0][1] {
		t.Fatalf("bad large-step expansion: %v", cron.fields[0])
	}
	for _, value := range []string{"+1 * * * *", "* * * * * *", "*/0 * * * *", "1,,2 * * * *", "MON * * * *"} {
		if _, err := parseCron(value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
	for _, value := range []string{"+1:30", "09:+1", "09:30:+1", "09:30:01.0000", "09:30:01."} {
		if _, _, _, _, err := parseClock(value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
}

func TestRecurrenceExhaustionRetainsReadableInactiveRecord(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := stamp("9999-12-31T23:59:00Z")
	task := Task{ID: "exhausted", SessionID: "a", Prompt: "last occurrence", Kind: "daily", Clock: "23:59", Timezone: "UTC", Next: target, Enabled: true, CreatedAt: stamp("2026-10-26T00:00:00Z")}
	if err = s.locked(context.Background(), func() error { return s.write([]Task{task}) }); err != nil {
		t.Fatal(err)
	}
	if err = s.tick(context.Background(), stamp("9999-12-31T23:59:59Z"), func(context.Context, Task) error { return nil }); err != nil {
		t.Fatal(err)
	}
	all, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Enabled || !all[0].Next.Equal(target) || all[0].LastError == "" {
		t.Fatalf("invalid exhausted record: %#v", all)
	}
	if _, err := Next(Task{Kind: "daily", Clock: "23:59", Timezone: "UTC"}, stamp("9999-12-31T23:59:59Z")); err == nil {
		t.Fatal("UTC year 10000 target accepted")
	}
}
