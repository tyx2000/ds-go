package schedule

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func runHostDrive(s *Store, now time.Time, deliver func(context.Context, DeliveryBatch) error) (batchDriveResult, error) {
	return hostOperation(context.Background(), s, func(ctx context.Context) (batchDriveResult, error) {
		return s.driveBatches(ctx, now, func() time.Time { return now }, deliver)
	})
}

func TestHostBatchGroupsRecurringPerSessionAndSharesReceipt(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := createOfficial(t, s, "a", `{"title":"First","prompt":"first","every_seconds":60}`)
	b := createOfficial(t, s, "b", `{"title":"Other session","prompt":"other","every_seconds":60}`)
	a2 := createOfficial(t, s, "a", `{"title":"Second","prompt":"second","every_seconds":120}`)
	once := createOfficial(t, s, "a", `{"title":"Once","prompt":"one shot","after_seconds":60}`)
	var batches []DeliveryBatch
	now := time.Now().Add(10 * time.Minute)
	_, err = runHostDrive(s, now, func(ctx context.Context, batch DeliveryBatch) error {
		batches = append(batches, batch)
		return s.AcknowledgeBatch(ctx, batch, fmt.Sprintf("accepted-message-%d", len(batches)))
	})
	if err != nil || len(batches) != 3 || len(batches[0].Tasks) != 2 || batches[0].Tasks[0].ID != a.ID || batches[0].Tasks[1].ID != a2.ID || batches[1].Tasks[0].ID != b.ID || batches[2].Tasks[0].ID != once.ID {
		t.Fatalf("batch order: %+v %v", batches, err)
	}
	if !strings.HasPrefix(batches[0].Text(), "[SCHEDULE REMINDER BATCH]\nThis is a scheduled message from the user\nreminders_json: ") || !strings.Contains(batches[0].Text(), `"reminder_prompt":"second"`) || !strings.HasPrefix(batches[2].Text(), "[SCHEDULE REMINDER]\n") {
		t.Fatal("incorrect official reminder framing")
	}
	deliveredAt := ""
	for _, v := range []View{a, a2} {
		history, err := s.History(context.Background(), "a", v.ID, 10, "")
		if err != nil || len(history.Records) != 1 || history.Records[0].MessageID != "accepted-message-1" {
			t.Fatalf("shared receipt: %+v %v", history, err)
		}
		if deliveredAt != "" && deliveredAt != history.Records[0].DeliveredAt {
			t.Fatal("batch receipts did not share their acceptance time")
		}
		deliveredAt = history.Records[0].DeliveredAt
	}
	remaining, _ := s.ListOfficial(context.Background(), "a")
	if len(remaining) != 2 {
		t.Fatal("one shot did not end independently")
	}
}

func TestHostPartialBatchAcknowledgmentKeepsSuccessfulTimerObligation(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first := createOfficial(t, s, "a", `{"title":"First","prompt":"p","every_seconds":60}`)
	second := createOfficial(t, s, "a", `{"title":"Second","prompt":"q","every_seconds":120}`)
	result, err := runHostDrive(s, time.Now().Add(10*time.Minute), func(ctx context.Context, batch DeliveryBatch) error {
		broken := batch
		broken.Tasks = append([]Task(nil), batch.Tasks...)
		broken.Tasks[1].Pending = "stale-pending-identity"
		return s.AcknowledgeBatch(ctx, broken, "shared-message")
	})
	if err != nil {
		t.Fatal(err)
	}
	history, err := s.History(context.Background(), "a", first.ID, 10, "")
	if err != nil || len(history.Records) != 1 {
		t.Fatalf("earlier member was rolled back after later failure: %+v %v", history, err)
	}
	missing, err := s.History(context.Background(), "a", second.ID, 10, "")
	if err != nil || len(missing.Records) != 0 {
		t.Fatalf("failed member was acknowledged: %+v %v", missing, err)
	}
	tasks, _ := s.List()
	var next time.Time
	for _, task := range tasks {
		if task.ID == first.ID {
			next = task.Next
		}
	}
	if result.next.IsZero() || !result.next.Equal(next) {
		t.Fatalf("successful member lost timer obligation: %+v next=%s", result, next)
	}
}

func TestHostFailedDeliveryHasNoAutomaticRetryTimer(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	createOfficial(t, s, "a", `{"title":"Fail once","prompt":"p","after_seconds":1}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := make(chan int, 4)
	var count atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- s.RunBatchesWithStart(ctx, nil, func(ctx context.Context, batch DeliveryBatch) error {
			n := int(count.Add(1))
			attempts <- n
			if n == 1 {
				return errors.New("fixture restoration failed")
			}
			return s.AcknowledgeBatch(ctx, batch, "accepted-after-management")
		})
	}()
	select {
	case <-attempts:
	case <-time.After(3 * time.Second):
		t.Fatal("first due reminder was not attempted")
	}
	select {
	case n := <-attempts:
		t.Fatalf("automatic retry %d", n)
	case <-time.After(1250 * time.Millisecond):
	}
	createOfficial(t, s, "other", `{"title":"Management wake","prompt":"later","after_seconds":3600}`)
	select {
	case n := <-attempts:
		if n != 2 {
			t.Fatalf("unexpected attempt %d", n)
		}
	case <-time.After(time.Second):
		t.Fatal("management change did not retry pending reminder")
	}
	cancel()
	if err := awaitError(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestHostDeliveryAndManagementShareFIFOAcrossSessions(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	other, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	first := createOfficial(t, s, "a", `{"title":"First","prompt":"p","after_seconds":60}`)
	second := createOfficial(t, s, "b", `{"title":"Second","prompt":"p","after_seconds":60}`)
	entered, release, secondEntered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	driven := make(chan error, 1)
	go func() {
		_, err := runHostDrive(s, time.Now().Add(time.Hour), func(ctx context.Context, batch DeliveryBatch) error {
			if batch.Tasks[0].ID == first.ID {
				close(entered)
				<-release
			} else if batch.Tasks[0].ID == second.ID {
				close(secondEntered)
			}
			return s.AcknowledgeBatch(ctx, batch, "message-"+batch.Tasks[0].ID)
		})
		driven <- err
	}()
	await(t, entered)
	written := make(chan error, 1)
	go func() {
		_, err := other.CreateOfficial(context.Background(), "c", CreateRequest{Title: "Queued create", Prompt: "q", TimingInput: TimingInput{AfterSeconds: pointer(int64(60))}})
		written <- err
	}()
	select {
	case <-secondEntered:
		t.Fatal("another Session overtook an unaccepted Host delivery")
	case err := <-written:
		t.Fatalf("management overtook delivery: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	if err := awaitError(t, driven); err != nil {
		t.Fatal(err)
	}
	await(t, secondEntered)
	if err := awaitError(t, written); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalZoneMatchesIntlAliasesAndStoredTargetsRemainStable(t *testing.T) {
	for raw, want := range map[string]string{"US/Eastern": "America/New_York", "america/new_york": "America/New_York", "Asia/Kolkata": "Asia/Calcutta", "Etc/UTC": "UTC", "etc/gmt": "UTC"} {
		got, err := canonicalZone(raw)
		if err != nil || got != want {
			t.Errorf("%s => %s %v wanted %s", raw, got, err, want)
		}
	}
	for _, raw := range []string{"utc", " UTC", "GMT+1", "../Asia/Shanghai", "Not/A_Zone"} {
		if _, err := canonicalZone(raw); err == nil {
			t.Errorf("accepted invalid zone %q", raw)
		}
	}
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	view := createOfficial(t, s, "a", `{"title":"Alias","prompt":"p","daily":{"time":"09:00:00","time_zone":"US/Eastern"}}`)
	if view.TimeZone != "America/New_York" {
		t.Fatalf("new zone was not canonicalized: %+v", view)
	}
	var target time.Time
	err = s.locked(context.Background(), func() error {
		tasks, err := s.read()
		if err != nil {
			return err
		}
		tasks[0].Timezone = "US/Eastern"
		target = tasks[0].Next
		return s.write(tasks)
	})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(strings.TrimSuffix(s.root, "/schedules"))
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := reopened.List()
	if err != nil || !tasks[0].Next.Equal(target) || tasks[0].Timezone != "US/Eastern" {
		t.Fatalf("stored alias or target rewritten: %+v %v", tasks, err)
	}
}
