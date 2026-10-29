package schedule

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type DeliveryBatch struct {
	SessionID  string
	DeliveryID string
	Tasks      []Task
}

var ErrNotDue = errors.New("reminder targets became future before admission")

func (b DeliveryBatch) Text() string {
	if len(b.Tasks) == 0 {
		return ""
	}
	if b.Tasks[0].Kind == "once" {
		t := b.Tasks[0]
		return "[SCHEDULE REMINDER]\nThis is a scheduled message from the user\nschedule_id_json: " + reminderJSON(t.ID) + "\noccurrence_at: " + instant(t.PendingAt) + "\nreminder_prompt_json: " + reminderJSON(t.Prompt)
	}
	type item struct {
		ID     string `json:"schedule_id"`
		At     string `json:"occurrence_at"`
		Prompt string `json:"reminder_prompt"`
	}
	items := make([]item, 0, len(b.Tasks))
	for _, t := range b.Tasks {
		items = append(items, item{t.ID, instant(t.PendingAt), t.Prompt})
	}
	return "[SCHEDULE REMINDER BATCH]\nThis is a scheduled message from the user\nreminders_json: " + reminderJSON(items)
}

func reminderJSON(value any) string {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	return strings.TrimSuffix(out.String(), "\n")
}

func deliveryBatch(tasks []Task) DeliveryBatch {
	if len(tasks) == 0 {
		return DeliveryBatch{}
	}
	ids := make([]string, len(tasks))
	for i, t := range tasks {
		ids[i] = t.Pending
	}
	digest := sha256.Sum256([]byte(strings.Join(ids, "\x00")))
	return DeliveryBatch{SessionID: tasks[0].SessionID, DeliveryID: "schedule-batch-" + hex.EncodeToString(digest[:16]), Tasks: tasks}
}

// RunBatchesWithStart is the terminal Host scheduler. A failed delivery has no
// retry timer. Only a management change, another future obligation, or restart
// requests another drive. Metadata polling detects external management writes;
// it never re-dispatches a task merely because time passed after its failure.
func (s *Store) RunBatchesWithStart(ctx context.Context, onStart func(context.Context) error, deliver func(context.Context, DeliveryBatch) error) error {
	release, err := daemonLock(filepath.Join(s.root, "daemon.lock"))
	if err != nil {
		return err
	}
	defer release()
	if deliver == nil {
		return errors.New("schedule delivery callback is required")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if onStart != nil {
		if err = onStart(ctx); err != nil {
			return err
		}
	}
	poll := time.NewTicker(250 * time.Millisecond)
	defer poll.Stop()
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	var timerC <-chan time.Time
	observed := scheduleFileStamp{}
	drive := true
	for {
		if drive {
			result, err := hostOperation(ctx, s, func(operationCtx context.Context) (batchDriveResult, error) {
				for {
					select {
					case <-s.managementWake:
						continue
					default:
						goto drained
					}
				}
			drained:
				result, err := s.driveBatches(operationCtx, time.Now().UTC(), time.Now, deliver)
				result.stamp = s.fileStamp()
				return result, err
			})
			if err != nil {
				return err
			}
			observed = result.stamp
			if timer != nil {
				timer.Stop()
				timer = nil
				timerC = nil
			}
			if !result.next.IsZero() {
				delay := time.Until(result.next)
				if delay < 0 {
					delay = 0
				}
				if delay > 2147483647*time.Millisecond {
					delay = 2147483647 * time.Millisecond
				}
				timer = time.NewTimer(delay)
				timerC = timer.C
			}
			drive = false
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.managementWake:
			drive = true
		case <-timerC:
			drive = true
			timerC = nil
		case <-poll.C:
			if s.fileStamp() != observed {
				drive = true
			}
		}
	}
}

type scheduleFileStamp struct {
	Exists   bool
	Modified int64
	Size     int64
}

func (s *Store) fileStamp() scheduleFileStamp {
	info, err := os.Stat(filepath.Join(s.root, "tasks.json"))
	if err != nil {
		return scheduleFileStamp{}
	}
	return scheduleFileStamp{true, info.ModTime().UnixNano(), info.Size()}
}

type batchDriveResult struct {
	next  time.Time
	stamp scheduleFileStamp
}

func (s *Store) driveBatches(ctx context.Context, scan time.Time, clock func() time.Time, deliver func(context.Context, DeliveryBatch) error) (batchDriveResult, error) {
	var tasks []Task
	if err := s.locked(ctx, func() error { var err error; tasks, err = s.read(); return err }); err != nil {
		return batchDriveResult{}, err
	}
	due := []Task{}
	for _, task := range tasks {
		if task.Enabled && !task.Next.After(scan) {
			due = append(due, task)
		}
	}
	handled, failed := map[string]bool{}, map[string]bool{}
	for _, task := range due {
		if err := ctx.Err(); err != nil {
			return batchDriveResult{}, err
		}
		if handled[task.ID] {
			continue
		}
		group := []Task{task}
		if task.Kind != "once" {
			group = nil
			for _, candidate := range due {
				if candidate.SessionID == task.SessionID && candidate.Kind != "once" {
					group = append(group, candidate)
				}
			}
		}
		for _, member := range group {
			handled[member.ID] = true
		}
		batch, err := s.claimBatch(ctx, group, clock().UTC(), false)
		if err != nil {
			return batchDriveResult{}, err
		}
		if len(batch.Tasks) == 0 {
			continue
		}
		err = deliver(ctx, batch)
		if ctx.Err() != nil {
			return batchDriveResult{}, ctx.Err()
		}
		if errors.Is(err, ErrNotDue) {
			continue
		}
		if settleErr := s.finishBatch(ctx, batch, clock().UTC(), err); settleErr != nil {
			return batchDriveResult{}, settleErr
		}
		if err != nil {
			if readErr := s.locked(ctx, func() error {
				current, readErr := s.read()
				if readErr != nil {
					return readErr
				}
				failedAt := clock().UTC()
				for _, member := range batch.Tasks {
					for _, task := range current {
						if task.ID == member.ID && task.Enabled && task.Pending == member.Pending && !task.Next.After(failedAt) {
							failed[task.ID] = true
						}
					}
				}
				return nil
			}); readErr != nil {
				return batchDriveResult{}, readErr
			}
			if warn := s.getOptions().Warn; warn != nil {
				warn(fmt.Errorf("reminders were not acknowledged: %w", err))
			}
		}
	}
	result := batchDriveResult{}
	err := s.locked(ctx, func() error {
		current, err := s.read()
		if err != nil {
			return err
		}
		for _, task := range current {
			if task.Enabled && !failed[task.ID] && (result.next.IsZero() || task.Next.Before(result.next)) {
				result.next = task.Next
			}
		}
		return nil
	})
	return result, err
}

func (s *Store) claimBatch(ctx context.Context, snapshots []Task, now time.Time, refresh bool) (DeliveryBatch, error) {
	claimed := []Task{}
	err := s.locked(ctx, func() error {
		tasks, err := s.read()
		if err != nil {
			return err
		}
		changed := false
		for _, snapshot := range snapshots {
			for i := range tasks {
				current := &tasks[i]
				if current.ID != snapshot.ID || current.SessionID != snapshot.SessionID || !current.Enabled || current.Next.After(now) {
					continue
				}
				if current.Pending == "" {
					current.Pending = id()
					changed = true
				}
				if current.PendingAt.IsZero() || refresh {
					current.PendingAt, err = latestOccurrence(*current, now)
					if err != nil {
						return err
					}
					changed = true
					next, nextErr := Next(*current, now)
					if current.Kind == "once" || nextErr != nil {
						next = time.Time{}
					}
					current.PendingNext = &next
				}
				if current.PendingPrompt == nil || refresh {
					prompt := current.Prompt
					current.PendingPrompt = &prompt
					changed = true
				}
				copy := *current
				copy.Prompt = *current.PendingPrompt
				claimed = append(claimed, copy)
				break
			}
		}
		if changed {
			return s.write(tasks)
		}
		return nil
	})
	return deliveryBatch(claimed), err
}

// RefreshBatch is used only after the Host proves this delivery identity has
// not yet been accepted. It recomputes latest missed occurrences after cold
// restoration while retaining the admission identity for crash deduplication.
func (s *Store) RefreshBatch(ctx context.Context, batch DeliveryBatch) (DeliveryBatch, error) {
	return s.claimBatch(ctx, batch.Tasks, time.Now().UTC(), true)
}

func (s *Store) AcknowledgeBatch(ctx context.Context, batch DeliveryBatch, messageID string) error {
	if messageID == "" || strings.TrimSpace(messageID) != messageID || batch.SessionID == "" || len(batch.Tasks) == 0 {
		return inputError("invalid_rule", "messageId must be non-empty without surrounding whitespace.")
	}
	_, err := hostOperation(ctx, s, func(ctx context.Context) (struct{}, error) {
		now := time.Now().UTC().Truncate(time.Millisecond)
		for _, member := range batch.Tasks {
			err := s.locked(ctx, func() error {
				tasks, err := s.read()
				if err != nil {
					return err
				}
				found := false
				for i := range tasks {
					current := &tasks[i]
					if current.ID != member.ID || current.SessionID != batch.SessionID {
						continue
					}
					found = true
					if current.LastDelivery != nil && current.LastDelivery.MessageID == messageID {
						return nil
					}
					if !current.Enabled || member.Pending == "" || current.Pending != member.Pending {
						return inputError("schedule_conflict", "The reminder changed before its delivery acknowledgment.")
					}
					appendReceipt(current, Receipt{ScheduledAt: instant(current.PendingAt), DeliveredAt: instant(now), MessageID: messageID}, s.getOptions())
					advanceTask(current, now)
					break
				}
				if !found {
					return inputError("schedule_not_found", "The reminder does not exist in this session.")
				}
				return s.write(tasks)
			})
			if err != nil {
				return struct{}{}, err
			}
		}
		return struct{}{}, nil
	})
	return err
}

func (s *Store) finishBatch(ctx context.Context, batch DeliveryBatch, now time.Time, deliveryErr error) error {
	return s.locked(ctx, func() error {
		tasks, err := s.read()
		if err != nil {
			return err
		}
		changed := false
		for _, member := range batch.Tasks {
			for i := range tasks {
				current := &tasks[i]
				if current.ID != member.ID || !current.Enabled || current.Pending != member.Pending {
					continue
				}
				changed = true
				if deliveryErr != nil {
					current.LastError = deliveryErr.Error()
				} else {
					current.EarlierRecordsUnavailable = true
					advanceTask(current, now)
				}
			}
		}
		if changed {
			return s.write(tasks)
		}
		return nil
	})
}
