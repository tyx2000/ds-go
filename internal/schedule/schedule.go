// Package schedule implements a durable host-wide reminder queue. Running the
// daemon is explicit: an exited terminal cannot execute scheduled work.
package schedule

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Task struct {
	ID                        string           `json:"id"`
	SessionID                 string           `json:"session_id"`
	Prompt                    string           `json:"prompt"`
	Kind                      string           `json:"kind"` // once, interval, daily, weekly, cron
	At                        time.Time        `json:"at,omitempty"`
	Every                     string           `json:"every,omitempty"`
	Clock                     string           `json:"clock,omitempty"`   // HH:MM[:SS[.sss]]
	Weekday                   int              `json:"weekday,omitempty"` // Sunday=0
	Cron                      string           `json:"cron,omitempty"`
	Timezone                  string           `json:"timezone"`
	Next                      time.Time        `json:"next"`
	Enabled                   bool             `json:"enabled"`
	CreatedAt                 time.Time        `json:"created_at"`
	Pending                   string           `json:"pending,omitempty"`
	PendingAt                 time.Time        `json:"pending_at,omitempty"`
	PendingPrompt             *string          `json:"pending_prompt,omitempty"`
	LastDelivered             time.Time        `json:"last_delivered,omitempty"`
	LastError                 string           `json:"last_error,omitempty"`
	Title                     string           `json:"title,omitempty"`
	Official                  bool             `json:"official,omitempty"`
	Selector                  string           `json:"selector,omitempty"`
	AfterSeconds              int64            `json:"after_seconds,omitempty"`
	EverySeconds              int64            `json:"every_seconds,omitempty"`
	Weekdays                  []int            `json:"weekdays,omitempty"`
	LastDelivery              *Receipt         `json:"last_delivery,omitempty"`
	DeliveryHistory           []DeliveryRecord `json:"delivery_history,omitempty"`
	HistoryKnown              bool             `json:"history_known,omitempty"`
	EarlierRecordsUnavailable bool             `json:"earlier_records_unavailable,omitempty"`
	EarlierRecordsPruned      bool             `json:"earlier_records_pruned,omitempty"`
}

type Store struct {
	root      string
	mu        sync.Mutex
	activeMu  sync.Mutex
	active    map[string]context.CancelFunc
	optionsMu sync.RWMutex
	options   Options
}

func Open(root string) (*Store, error) {
	root = filepath.Join(root, "schedules")
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	s := &Store{root: root, active: make(map[string]context.CancelFunc), options: defaultOptions()}
	if err := s.locked(context.Background(), func() error { _, err := s.read(); return err }); err != nil {
		return nil, err
	}
	return s, nil
}
func id() string { var b [10]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func (s *Store) locked(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lock := filepath.Join(s.root, "write.lock")
	var release func()
	for {
		var err error
		release, err = daemonLock(lock)
		if err == nil {
			break
		}
		if !lockBusy(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}
func (s *Store) read() ([]Task, error) {
	b, err := os.ReadFile(filepath.Join(s.root, "tasks.json"))
	if errors.Is(err, os.ErrNotExist) {
		return []Task{}, nil
	}
	if err != nil {
		return nil, err
	}
	var tasks []Task
	if err = json.Unmarshal(b, &tasks); err != nil {
		return nil, fmt.Errorf("invalid schedule store: %w", err)
	}
	if tasks == nil {
		return nil, errors.New("invalid schedule store: expected task array")
	}
	seen := map[string]bool{}
	for _, t := range tasks {
		if t.ID == "" || seen[t.ID] || t.SessionID == "" || strings.TrimSpace(t.Prompt) == "" || t.CreatedAt.IsZero() || t.Next.IsZero() || !supportedInstant(t.Next) {
			return nil, errors.New("invalid schedule store: invalid task identity, content, or target")
		}
		seen[t.ID] = true
		if (t.Pending == "") != t.PendingAt.IsZero() {
			return nil, errors.New("invalid schedule store: incomplete pending delivery")
		}
		if _, err := time.LoadLocation(t.Timezone); err != nil {
			return nil, fmt.Errorf("invalid schedule store timezone: %w", err)
		}
		if err := validateRule(t); err != nil {
			return nil, fmt.Errorf("invalid schedule store timing: %w", err)
		}
		if err := validateOfficialTask(t); err != nil {
			return nil, fmt.Errorf("invalid schedule store: %w", err)
		}
	}
	return tasks, nil
}

func validateRule(t Task) error {
	switch t.Kind {
	case "once":
		if t.At.IsZero() {
			return errors.New("one-shot target is missing")
		}
	case "interval":
		_, err := intervalNanos(t)
		if err != nil {
			return errors.New("invalid interval")
		}
	case "daily", "weekly":
		if _, _, _, _, err := parseClock(t.Clock); err != nil {
			return err
		}
		if t.Kind == "weekly" {
			if err := validateWeekdays(t); err != nil {
				return err
			}
		}
	case "cron":
		if _, err := parseCron(t.Cron); err != nil {
			return err
		}
	default:
		return errors.New("invalid schedule kind")
	}
	return nil
}
func (s *Store) write(tasks []Task) error {
	b, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.root, "tasks-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	if x := f.Close(); err == nil {
		err = x
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(s.root, "tasks.json")); err != nil {
		return err
	}
	dir, err := os.Open(s.root)
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
func (s *Store) List() ([]Task, error) {
	var out []Task
	err := s.locked(context.Background(), func() error { var err error; out, err = s.read(); return err })
	return out, err
}
func (s *Store) Create(t Task) (Task, error) {
	// Legacy callers provide timing/content only, never durable receipt fields.
	t = Task{SessionID: t.SessionID, Prompt: t.Prompt, Kind: t.Kind, At: t.At, Every: t.Every, EverySeconds: t.EverySeconds, Clock: t.Clock, Weekday: t.Weekday, Weekdays: append([]int{}, t.Weekdays...), Cron: t.Cron, Timezone: t.Timezone, Title: t.Title}
	if strings.TrimSpace(t.Prompt) == "" || t.SessionID == "" {
		return t, errors.New("session_id and prompt are required")
	}
	if t.Title != "" {
		var err error
		t.Title, err = title(t.Title)
		if err != nil {
			return t, err
		}
	}
	if t.Timezone == "" {
		t.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(t.Timezone); err != nil {
		return t, err
	}
	t.ID = id()
	t.CreatedAt = time.Now().UTC()
	t.Enabled = true
	t.Pending = ""
	t.PendingAt = time.Time{}
	t.LastDelivered = time.Time{}
	t.LastError = ""
	t.HistoryKnown = true
	var err error
	t.Next, err = Next(t, t.CreatedAt)
	if err != nil {
		return t, err
	}
	err = s.locked(context.Background(), func() error {
		if err := s.checkSession(context.Background(), t.SessionID); err != nil {
			return err
		}
		tasks, err := s.read()
		if err != nil {
			return err
		}
		return s.write(append(tasks, t))
	})
	return t, err
}
func (s *Store) Cancel(id string) error {
	err := s.locked(context.Background(), func() error {
		tasks, err := s.read()
		if err != nil {
			return err
		}
		for i := range tasks {
			if tasks[i].ID == id {
				tasks[i].Enabled = false
				tasks[i].Pending = ""
				tasks[i].PendingAt = time.Time{}
				tasks[i].PendingPrompt = nil
				return s.write(tasks)
			}
		}
		return errors.New("schedule not found")
	})
	if err == nil {
		s.activeMu.Lock()
		if cancel := s.active[id]; cancel != nil {
			cancel()
		}
		s.activeMu.Unlock()
	}
	return err
}

// Next returns the next occurrence strictly after after. Cron's day-of-month
// and day-of-week fields use the usual OR rule when both are constrained.
func Next(t Task, after time.Time) (time.Time, error) {
	next, err := nextOccurrence(t, after)
	if err == nil && !supportedInstant(next) {
		return time.Time{}, errors.New("schedule exceeds the supported UTC year range 0001..9999")
	}
	return next, err
}
func supportedInstant(t time.Time) bool { year := t.UTC().Year(); return year >= 1 && year <= 9999 }
func nextOccurrence(t Task, after time.Time) (time.Time, error) {
	loc, err := time.LoadLocation(t.Timezone)
	if err != nil {
		return time.Time{}, err
	}
	switch t.Kind {
	case "once":
		if !t.At.After(after) {
			return time.Time{}, errors.New("one-shot time must be in the future")
		}
		return t.At.UTC(), nil
	case "interval":
		step, err := intervalNanos(t)
		if err != nil {
			return time.Time{}, errors.New("interval must be a duration of at least 1m")
		}
		anchor := t.CreatedAt
		if anchor.IsZero() {
			anchor = after
		}
		first := addInterval(anchor, step)
		if after.Before(first) {
			return first.UTC(), nil
		}
		return addInterval(latestIntervalNanos(first, after, step), step).UTC(), nil
	case "daily", "weekly":
		h, m, sec, nsec, err := parseClock(t.Clock)
		if err != nil {
			return time.Time{}, err
		}
		if t.Kind == "weekly" {
			if err := validateWeekdays(t); err != nil {
				return time.Time{}, err
			}
		}
		a := after.In(loc)
		for d := 0; d < 9; d++ {
			day := a.AddDate(0, 0, d)
			candidate, ok := localOccurrence(day, loc, h, m, sec, nsec)
			if !ok {
				continue
			}
			if candidate.After(after) && (t.Kind == "daily" || matchesWeekday(t, candidate.Weekday())) {
				return candidate.UTC(), nil
			}
		}
		return time.Time{}, errors.New("cannot resolve next local occurrence")
	case "cron":
		return cronOccurrence(t, after, false)
	default:
		return time.Time{}, errors.New("kind must be once, interval, daily, weekly, or cron")
	}
}

func parseClock(clock string) (h, m, sec, nsec int, err error) {
	parts := strings.Split(clock, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return 0, 0, 0, 0, errors.New("clock must be HH:MM or HH:MM:SS[.sss]")
	}
	if len(parts[0]) != 2 || len(parts[1]) != 2 {
		return 0, 0, 0, 0, errors.New("invalid clock")
	}
	h, err = unsigned(parts[0])
	if err != nil {
		return
	}
	m, err = unsigned(parts[1])
	if err != nil {
		return
	}
	if len(parts) == 3 {
		whole, fraction, hasFraction := strings.Cut(parts[2], ".")
		if len(whole) != 2 {
			return 0, 0, 0, 0, errors.New("invalid clock seconds")
		}
		sec, err = unsigned(whole)
		if err != nil {
			return
		}
		if hasFraction {
			if len(fraction) < 1 || len(fraction) > 3 {
				return 0, 0, 0, 0, errors.New("clock fractions require one to three digits")
			}
			for _, r := range fraction {
				if r < '0' || r > '9' {
					return 0, 0, 0, 0, errors.New("invalid clock fraction")
				}
			}
			nsec, err = strconv.Atoi(fraction + strings.Repeat("0", 9-len(fraction)))
			if err != nil {
				return
			}
		}
	}
	if h < 0 || h > 23 || m < 0 || m > 59 || sec < 0 || sec > 59 {
		err = errors.New("invalid clock")
	}
	return
}

func unsigned(value string) (int, error) {
	if value == "" {
		return 0, errors.New("empty integer")
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, errors.New("invalid unsigned integer")
		}
	}
	return strconv.Atoi(value)
}

// Go's time.Date may select either side of an overlap. Resolve all nearby UTC
// offsets and keep only the earliest matching instant; missing local times have
// no matching instant. This also handles half-hour and date-line transitions.
func localOccurrence(day time.Time, loc *time.Location, h, m, sec, nsec int) (time.Time, bool) {
	year, month, date := day.In(loc).Date()
	return localFieldsOccurrence(year, month, date, loc, h, m, sec, nsec)
}
func localFieldsOccurrence(year int, month time.Month, date int, loc *time.Location, h, m, sec, nsec int) (time.Time, bool) {
	wall := time.Date(year, month, date, h, m, sec, nsec, time.UTC)
	guess := time.Date(year, month, date, h, m, sec, nsec, loc)
	offsets := map[int]bool{}
	for delta := -48 * time.Hour; delta <= 48*time.Hour; delta += 6 * time.Hour {
		_, offset := guess.Add(delta).In(loc).Zone()
		offsets[offset] = true
	}
	var earliest time.Time
	for offset := range offsets {
		candidate := wall.Add(-time.Duration(offset) * time.Second)
		local := candidate.In(loc)
		cy, cm, cd := local.Date()
		if cy != year || cm != month || cd != date || local.Hour() != h || local.Minute() != m || local.Second() != sec || local.Nanosecond() != nsec {
			continue
		}
		if earliest.IsZero() || candidate.Before(earliest) {
			earliest = candidate
		}
	}
	return earliest, !earliest.IsZero()
}
func earlierOverlap(instant time.Time, loc *time.Location) bool {
	local := instant.In(loc)
	earliest, ok := localOccurrence(local, loc, local.Hour(), local.Minute(), local.Second(), local.Nanosecond())
	return ok && instant.Equal(earliest)
}

func latestInterval(first, now time.Time, interval time.Duration) time.Time {
	return latestIntervalNanos(first, now, big.NewInt(int64(interval)))
}
func latestIntervalNanos(first, now time.Time, interval *big.Int) time.Time {
	// time.Time.Sub saturates after about 290 years. Unix seconds and big.Int
	// preserve the fixed-rate alignment across the entire supported year range.
	difference := big.NewInt(now.Unix() - first.Unix())
	difference.Mul(difference, big.NewInt(1e9))
	difference.Add(difference, big.NewInt(int64(now.Nanosecond()-first.Nanosecond())))
	steps := new(big.Int).Quo(difference, interval)
	offset := steps.Mul(steps, interval)
	seconds, nanos := new(big.Int), new(big.Int)
	seconds.QuoRem(offset, big.NewInt(1e9), nanos)
	return time.Unix(first.Unix()+seconds.Int64(), int64(first.Nanosecond())+nanos.Int64()).UTC()
}

func latestOccurrence(t Task, now time.Time) (time.Time, error) {
	if t.Next.After(now) {
		return time.Time{}, errors.New("schedule is not due")
	}
	switch t.Kind {
	case "once":
		return t.Next, nil
	case "interval":
		step, err := intervalNanos(t)
		if err != nil {
			return time.Time{}, errors.New("invalid interval")
		}
		return latestIntervalNanos(t.Next, now, step), nil
	case "daily", "weekly":
		loc, err := time.LoadLocation(t.Timezone)
		if err != nil {
			return time.Time{}, err
		}
		h, m, sec, nsec, err := parseClock(t.Clock)
		if err != nil {
			return time.Time{}, err
		}
		local := now.In(loc)
		for d := 0; d < 16; d++ {
			day := local.AddDate(0, 0, -d)
			if t.Kind == "weekly" && !matchesWeekday(t, day.Weekday()) {
				continue
			}
			candidate, ok := localOccurrence(day, loc, h, m, sec, nsec)
			if ok && !candidate.After(now) && !candidate.Before(t.Next) {
				return candidate.UTC(), nil
			}
		}
		return t.Next, nil // A committed target survives later timezone-data changes.
	case "cron":
		return cronOccurrence(t, now, true)
	default:
		return time.Time{}, errors.New("invalid schedule kind")
	}
}

type cronExpr struct {
	fields [5]map[int]bool
	all    [5]bool
}

func parseCron(expr string) (cronExpr, error) {
	var c cronExpr
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return c, errors.New("cron requires five fields")
	}
	mins := []int{0, 0, 1, 1, 0}
	maxs := []int{59, 23, 31, 12, 7}
	for i, p := range parts {
		c.fields[i] = map[int]bool{}
		c.all[i] = strings.HasPrefix(p, "*")
		for _, piece := range strings.Split(p, ",") {
			base, stepText, hasStep := strings.Cut(piece, "/")
			step := 1
			if hasStep {
				var err error
				step, err = unsigned(stepText)
				if err != nil || step < 1 {
					return c, errors.New("invalid cron step")
				}
			}
			lo, hi := mins[i], maxs[i]
			if base != "*" {
				a, b, hasRange := strings.Cut(base, "-")
				var err error
				lo, err = unsigned(a)
				if err != nil {
					return c, errors.New("invalid cron value")
				}
				hi = lo
				if hasRange {
					hi, err = unsigned(b)
					if err != nil {
						return c, errors.New("invalid cron range")
					}
				} else if hasStep {
					hi = maxs[i]
				}
			}
			if lo < mins[i] || hi > maxs[i] || lo > hi {
				return c, errors.New("cron value out of range")
			}
			for n := lo; n <= hi; {
				if i == 4 && n == 7 {
					c.fields[i][0] = true
				} else {
					c.fields[i][n] = true
				}
				if step > hi-n {
					break
				}
				n += step
			}
		}
	}
	return c, nil
}
func (c cronExpr) match(t time.Time) bool {
	if !c.fields[0][t.Minute()] || !c.fields[1][t.Hour()] || !c.fields[3][int(t.Month())] {
		return false
	}
	dom, dow := c.fields[2][t.Day()], c.fields[4][int(t.Weekday())]
	if !c.all[2] && !c.all[4] {
		return dom || dow
	}
	return dom && dow
}

// Run retains a stable pending delivery id across failures and restarts. deliver
// must deduplicate this id against the session log before accepting a follow-up.
func (s *Store) Run(ctx context.Context, deliver func(context.Context, Task) error) error {
	return s.RunWithStart(ctx, nil, deliver)
}

// RunWithStart runs recovery only after acquiring the unique daemon lock.
// Deliver acknowledges durable inbox acceptance, not model execution. It must
// honor cancellation, and deduplicate the persisted Pending id against the log.
func (s *Store) RunWithStart(ctx context.Context, onStart func(context.Context) error, deliver func(context.Context, Task) error) error {
	// Kernel advisory lock is held by the daemon for its lifetime. See lock_*.go.
	release, err := daemonLock(filepath.Join(s.root, "daemon.lock"))
	if err != nil {
		return err
	}
	defer release()
	if deliver == nil {
		return errors.New("schedule delivery callback is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if onStart != nil {
		if err = onStart(ctx); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	failures := make(chan error, 1)
	dispatch := func() {
		now := time.Now().UTC()
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := s.tickAt(ctx, now, time.Now, deliver); err != nil && !errors.Is(err, context.Canceled) {
				select {
				case failures <- err:
				default:
				}
			}
		}()
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	dispatch()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-failures:
			return err
		case <-ticker.C:
			dispatch()
		}
	}
}
func (s *Store) tick(ctx context.Context, now time.Time, deliver func(context.Context, Task) error) error {
	return s.tickAt(ctx, now, func() time.Time { return now }, deliver)
}
func (s *Store) tickAt(ctx context.Context, now time.Time, clock func() time.Time, deliver func(context.Context, Task) error) error {
	var due []Task
	err := s.locked(ctx, func() error {
		tasks, err := s.read()
		if err != nil {
			return err
		}
		for i := range tasks {
			t := &tasks[i]
			if !t.Enabled || t.Next.After(now) {
				continue
			}
			due = append(due, *t)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.SliceStable(due, func(i, j int) bool {
		if due[i].Next.Equal(due[j].Next) {
			return due[i].ID < due[j].ID
		}
		return due[i].Next.Before(due[j].Next)
	})
	groups := map[string][]Task{}
	for _, task := range due {
		groups[task.SessionID] = append(groups[task.SessionID], task)
	}
	errorsOut := make(chan error, len(groups))
	var workers sync.WaitGroup
	for session, tasks := range groups {
		session, tasks := session, tasks
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := s.deliverSession(ctx, session, tasks, clock, deliver); err != nil {
				errorsOut <- err
			}
		}()
	}
	workers.Wait()
	close(errorsOut)
	var failures []error
	for err := range errorsOut {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func lockName(kind, value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%s-%x.lock", kind, sum[:])
}

func (s *Store) deliverSession(ctx context.Context, session string, tasks []Task, clock func() time.Time, deliver func(context.Context, Task) error) error {
	release, err := daemonLock(filepath.Join(s.root, lockName("session", session)))
	if lockBusy(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer release()
	for _, snapshot := range tasks {
		if err := s.deliverTask(ctx, snapshot, clock, deliver); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) deliverTask(ctx context.Context, snapshot Task, clock func() time.Time, deliver func(context.Context, Task) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	release, err := daemonLock(filepath.Join(s.root, lockName("task", snapshot.ID)))
	if lockBusy(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer release()
	deliveryCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.activeMu.Lock()
	s.active[snapshot.ID] = cancel
	s.activeMu.Unlock()
	defer func() { s.activeMu.Lock(); delete(s.active, snapshot.ID); s.activeMu.Unlock() }()
	var task Task
	admitted := false
	err = s.locked(deliveryCtx, func() error {
		if err := deliveryCtx.Err(); err != nil {
			return err
		}
		tasks, err := s.read()
		if err != nil {
			return err
		}
		now := clock().UTC()
		for i := range tasks {
			current := &tasks[i]
			if current.ID != snapshot.ID || !current.Enabled || current.Next.After(now) {
				continue
			}
			if current.Pending == "" {
				current.Pending = id()
				current.PendingAt, err = latestOccurrence(*current, now)
				if err != nil {
					return err
				}
			}
			if current.PendingPrompt == nil {
				value := current.Prompt
				current.PendingPrompt = &value
				if err = s.write(tasks); err != nil {
					return err
				}
			}
			task = *current
			if current.PendingPrompt != nil {
				task.Prompt = *current.PendingPrompt
			}
			admitted = true
			return nil
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !admitted {
		return nil
	}
	if err = deliveryCtx.Err(); err != nil {
		return nil
	}
	// A second Store/process can cancel a task while a callback is restoring a
	// Session. Observe its committed state without holding the writer lock across
	// the callback. Already accepted inbox messages remain in their Session.
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-deliveryCtx.Done():
				return
			case <-ticker.C:
				err := s.locked(deliveryCtx, func() error {
					current, err := s.read()
					if err != nil {
						return err
					}
					for _, t := range current {
						if t.ID == task.ID && t.Enabled && t.Pending == task.Pending {
							return nil
						}
					}
					return context.Canceled
				})
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	deliveryErr := deliver(deliveryCtx, task)
	cancel()
	<-watchDone
	// Settlement must still persist on normal callback completion; parent daemon
	// cancellation leaves the stable pending id for recovery instead.
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.locked(ctx, func() error {
		tasks, err := s.read()
		if err != nil {
			return err
		}
		for i := range tasks {
			current := &tasks[i]
			if current.ID != task.ID || !current.Enabled || current.Pending != task.Pending {
				continue
			}
			if deliveryErr != nil {
				current.LastError = deliveryErr.Error()
			} else {
				now := clock().UTC()
				current.EarlierRecordsUnavailable = true
				current.LastError = ""
				current.LastDelivered = now
				current.Pending = ""
				current.PendingAt = time.Time{}
				current.PendingPrompt = nil
				if current.Kind == "once" {
					current.Enabled = false
				} else {
					next, advanceErr := Next(*current, now)
					if advanceErr != nil {
						current.Enabled = false
						current.LastError = advanceErr.Error()
					} else {
						current.Next = next
					}
				}
			}
			return s.write(tasks)
		}
		return nil
	})
}
