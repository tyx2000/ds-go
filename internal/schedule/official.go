package schedule

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

const maxSafeInteger int64 = 9007199254740991
const MaxTitleLength = 120

type InputError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *InputError) Error() string         { return e.Code + ": " + e.Message }
func inputError(code, message string) error { return &InputError{Code: code, Message: message} }

type Options struct {
	DeliveryHistoryDays    int
	DeliveryHistoryRecords int
	// CheckSession is called inside the management transaction for create/update.
	// Return *InputError for public admission failures; other errors remain private.
	CheckSession func(context.Context, string) error
	Warn         func(error)
}

func defaultOptions() Options { return Options{DeliveryHistoryDays: 30, DeliveryHistoryRecords: 200} }
func (s *Store) SetOptions(options Options) error {
	if options.DeliveryHistoryDays == 0 {
		options.DeliveryHistoryDays = 30
	}
	if options.DeliveryHistoryRecords == 0 {
		options.DeliveryHistoryRecords = 200
	}
	if options.DeliveryHistoryDays < 1 || options.DeliveryHistoryDays > 3650 || options.DeliveryHistoryRecords < 1 || options.DeliveryHistoryRecords > 10000 {
		return inputError("invalid_rule", "Delivery history must retain 1..3650 days and 1..10000 records.")
	}
	s.optionsMu.Lock()
	s.options = options
	s.optionsMu.Unlock()
	return nil
}
func (s *Store) getOptions() Options {
	s.optionsMu.RLock()
	defer s.optionsMu.RUnlock()
	return s.options
}
func (s *Store) checkSession(ctx context.Context, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sessionID == "" || strings.TrimSpace(sessionID) != sessionID {
		return inputError("invalid_rule", "Session identity must be non-empty without surrounding whitespace.")
	}
	if check := s.getOptions().CheckSession; check != nil {
		return check(ctx, sessionID)
	}
	return nil
}

type DailyInput struct {
	Time     string `json:"time"`
	TimeZone string `json:"time_zone"`
}
type WeeklyInput struct {
	Time     string `json:"time"`
	TimeZone string `json:"time_zone"`
	Weekdays []int  `json:"weekdays"`
}
type CronInput struct {
	Expression string `json:"expression"`
	TimeZone   string `json:"time_zone"`
}
type LocalAtInput struct {
	Date     string `json:"date"`
	Time     string `json:"time"`
	TimeZone string `json:"time_zone"`
}
type TimingInput struct {
	AfterSeconds *int64          `json:"after_seconds,omitempty"`
	At           json.RawMessage `json:"at,omitempty"`
	EverySeconds *int64          `json:"every_seconds,omitempty"`
	Daily        *DailyInput     `json:"daily,omitempty"`
	Weekly       *WeeklyInput    `json:"weekly,omitempty"`
	Cron         *CronInput      `json:"cron,omitempty"`
}
type CreateRequest struct {
	Prompt string `json:"prompt"`
	Title  string `json:"title"`
	TimingInput
}
type UpdatePatch struct {
	Title  *string      `json:"title,omitempty"`
	Prompt *string      `json:"prompt,omitempty"`
	Timing *TimingInput `json:"timing,omitempty"`
}

type Record struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Title        string `json:"title"`
	Prompt       string `json:"prompt"`
	AfterSeconds int64  `json:"afterSeconds,omitempty"`
	EverySeconds int64  `json:"everySeconds,omitempty"`
	Time         string `json:"time,omitempty"`
	TimeZone     string `json:"timeZone,omitempty"`
	Weekdays     []int  `json:"weekdays,omitempty"`
	Expression   string `json:"expression,omitempty"`
	ScheduledAt  string `json:"scheduledAt"`
}
type View struct {
	Record
	State        string `json:"state"`
	DeliveryMode string `json:"deliveryMode"`
}
type CatalogEntry struct {
	Record
	SessionID    string   `json:"sessionId"`
	Status       string   `json:"status"`
	LastDelivery *Receipt `json:"lastDelivery,omitempty"`
}
type UpdateResult struct {
	ID      string  `json:"id"`
	Updated bool    `json:"updated"`
	Code    string  `json:"code,omitempty"`
	Record  *Record `json:"record,omitempty"`
	View    *View   `json:"-"`
	Message string  `json:"message,omitempty"`
}

func (r UpdateResult) MarshalJSON() ([]byte, error) {
	if r.Code == "subagent_session" {
		return json.Marshal(struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}{r.Code, r.Message})
	}
	type plain UpdateResult
	return json.Marshal(plain(r))
}

type DeleteResult struct {
	ID      string `json:"id"`
	Deleted bool   `json:"deleted"`
	Code    string `json:"code,omitempty"`
}
type Receipt struct {
	ScheduledAt string `json:"scheduledAt"`
	DeliveredAt string `json:"deliveredAt"`
	MessageID   string `json:"messageId"`
}
type DeliveryRecord struct {
	Receipt
	Prompt *string `json:"prompt,omitempty"`
}
type RetentionBounds struct {
	Days    int `json:"days"`
	Records int `json:"records"`
}
type HistoryPage struct {
	ID                        string           `json:"id"`
	Code                      string           `json:"code,omitempty"`
	Records                   []DeliveryRecord `json:"records"`
	EarlierRecordsUnavailable bool             `json:"earlierRecordsUnavailable"`
	EarlierRecordsPruned      bool             `json:"earlierRecordsPruned"`
	Retention                 RetentionBounds  `json:"retention"`
	NextBefore                string           `json:"nextBefore,omitempty"`
}

func (p HistoryPage) MarshalJSON() ([]byte, error) {
	if p.Code != "" {
		return json.Marshal(struct {
			ID   string `json:"id"`
			Code string `json:"code"`
		}{p.ID, p.Code})
	}
	type plain HistoryPage
	if p.Records == nil {
		p.Records = []DeliveryRecord{}
	}
	return json.Marshal(plain(p))
}
func identity(id string) error {
	if id == "" || strings.TrimSpace(id) != id {
		return inputError("invalid_rule", "Schedule identity must be non-empty without surrounding whitespace.")
	}
	return nil
}

func instant(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
func taskRecord(t Task) Record {
	kind := t.Kind
	if kind == "once" {
		kind = "at"
		if t.Selector == "after" {
			kind = "after"
		}
	}
	if kind == "interval" {
		kind = "every"
	}
	r := Record{ID: t.ID, Kind: kind, Title: t.Title, Prompt: t.Prompt, ScheduledAt: instant(t.Next)}
	switch kind {
	case "after":
		r.AfterSeconds = t.AfterSeconds
	case "every":
		r.EverySeconds = t.EverySeconds
		if r.EverySeconds == 0 {
			if d, err := time.ParseDuration(t.Every); err == nil {
				r.EverySeconds = int64(d / time.Second)
			}
		}
	case "daily", "weekly":
		r.Time = normalizedClock(t.Clock)
		r.TimeZone = t.Timezone
		if kind == "weekly" {
			r.Weekdays = append([]int{}, t.Weekdays...)
			if len(r.Weekdays) == 0 {
				day := t.Weekday
				if day == 0 {
					day = 7
				}
				r.Weekdays = []int{day}
			}
		}
	case "cron":
		r.Expression = t.Cron
		r.TimeZone = t.Timezone
	}
	return r
}
func taskView(t Task, now time.Time) View {
	state := "scheduled"
	if !t.Next.After(now) {
		state = "overdue"
	}
	return View{Record: taskRecord(t), State: state, DeliveryMode: "host"}
}

func title(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || !utf8.ValidString(value) {
		return "", inputError("invalid_prompt", "title is required and must be non-empty after trimming.")
	}
	if len(utf16.Encode([]rune(value))) > MaxTitleLength {
		return "", inputError("invalid_prompt", "title must be at most 120 characters.")
	}
	return value, nil
}
func prompt(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || !utf8.ValidString(value) {
		return "", inputError("invalid_prompt", "prompt must be non-empty after trimming.")
	}
	return value, nil
}
func (t TimingInput) selectorCount() int {
	n := 0
	if t.AfterSeconds != nil {
		n++
	}
	if len(t.At) > 0 {
		n++
	}
	if t.EverySeconds != nil {
		n++
	}
	if t.Daily != nil {
		n++
	}
	if t.Weekly != nil {
		n++
	}
	if t.Cron != nil {
		n++
	}
	return n
}

var zoneShape = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+.-]*(?:/[A-Za-z0-9_+.-]+)+$`)
var strictClock = regexp.MustCompile(`^(?:[01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](?:\.[0-9]{1,3})?$`)
var strictOffset = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T(?:[01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](?:\.[0-9]{1,3})?(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])$`)
var strictDate = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

func canonicalZone(zone string) (string, error) {
	if zone != "UTC" && (!zoneShape.MatchString(zone) || strings.TrimSpace(zone) != zone) {
		return "", inputError("invalid_time_zone", "time_zone must be UTC or a valid IANA Area/Location name.")
	}
	canonical, known := canonicalZones[strings.ToLower(zone)]
	if !known {
		return "", inputError("invalid_time_zone", "time_zone must be UTC or a valid IANA Area/Location name.")
	}
	if _, err := time.LoadLocation(canonical); err != nil {
		return "", inputError("invalid_time_zone", "time_zone must be UTC or a valid IANA Area/Location name.")
	}
	return canonical, nil
}
func normalizedClock(clock string) string {
	h, m, sec, nsec, err := parseClock(clock)
	if err != nil {
		return clock
	}
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, sec, nsec/1e6)
}
func officialClock(clock string) (string, error) {
	if !strictClock.MatchString(clock) {
		return "", inputError("invalid_rule", "time must use HH:mm:ss with optional one-to-three fractional digits.")
	}
	return normalizedClock(clock), nil
}
func parseAt(raw json.RawMessage) (time.Time, error) {
	var offset string
	if len(raw) > 0 && raw[0] == '"' {
		if err := json.Unmarshal(raw, &offset); err != nil {
			return time.Time{}, inputError("invalid_rule", "Invalid at value.")
		}
		if !strictOffset.MatchString(offset) || strings.HasSuffix(offset, "-00:00") {
			return time.Time{}, inputError("invalid_rule", "at must use a real RFC 3339 date-time with an explicit offset.")
		}
		v, err := time.Parse(time.RFC3339Nano, offset)
		if err != nil || v.Year() == 0 {
			return time.Time{}, inputError("invalid_rule", "at must use a real RFC 3339 date-time with an explicit offset.")
		}
		return v.UTC(), nil
	}
	var local LocalAtInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&local); err != nil {
		return time.Time{}, inputError("invalid_rule", "at must be an absolute instant or a local date, time, and time_zone.")
	}
	if !strictDate.MatchString(local.Date) {
		return time.Time{}, inputError("invalid_rule", "Local at requires a real YYYY-MM-DD date.")
	}
	day, err := time.Parse("2006-01-02", local.Date)
	if err != nil || day.Year() == 0 {
		return time.Time{}, inputError("invalid_rule", "Local at requires a real YYYY-MM-DD date.")
	}
	clock, err := officialClock(local.Time)
	if err != nil {
		return time.Time{}, err
	}
	zone, err := canonicalZone(local.TimeZone)
	if err != nil {
		return time.Time{}, err
	}
	loc, _ := time.LoadLocation(zone)
	h, m, sec, nsec, _ := parseClock(clock)
	// day carries calendar fields in UTC; localOccurrence needs that same date
	// represented in the chosen location, rather than a shifted UTC midnight.
	date := time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, loc)
	candidate, ok := localOccurrence(date, loc, h, m, sec, nsec)
	if !ok || candidate.In(loc).Format("2006-01-02") != local.Date {
		return time.Time{}, inputError("invalid_rule", "The local at time does not exist in the selected time zone.")
	}
	return candidate.UTC(), nil
}

func prepareOfficial(sessionID string, req CreateRequest, now time.Time) (Task, error) {
	var err error
	t := Task{SessionID: sessionID, Official: true, CreatedAt: now.UTC().Truncate(time.Millisecond), Enabled: true, Timezone: "UTC", HistoryKnown: true}
	t.Title, err = title(req.Title)
	if err != nil {
		return t, err
	}
	t.Prompt, err = prompt(req.Prompt)
	if err != nil {
		return t, err
	}
	if req.selectorCount() != 1 {
		return t, inputError("invalid_selector", "schedule_create accepts exactly one of after_seconds, at, every_seconds, daily, weekly, or cron.")
	}
	switch {
	case req.AfterSeconds != nil:
		seconds := *req.AfterSeconds
		if seconds < 1 || seconds > maxSafeInteger {
			return t, inputError("invalid_rule", "after_seconds must be a positive safe integer.")
		}
		t.Kind = "once"
		t.Selector = "after"
		t.AfterSeconds = seconds
		t.At = time.Unix(now.Unix()+seconds, int64(now.Nanosecond())).UTC().Truncate(time.Millisecond)
	case len(req.At) > 0:
		t.Kind = "once"
		t.Selector = "at"
		t.At, err = parseAt(req.At)
	case req.EverySeconds != nil:
		seconds := *req.EverySeconds
		if seconds < -maxSafeInteger || seconds > maxSafeInteger {
			return t, inputError("invalid_rule", "every_seconds must be a safe integer.")
		}
		if seconds < 60 {
			return t, inputError("frequency_too_high", "every_seconds must be at least 60.")
		}
		t.Kind = "interval"
		t.Selector = "every"
		t.EverySeconds = seconds
		t.Every = fmt.Sprintf("%ds", seconds)
	case req.Daily != nil:
		t.Kind = "daily"
		t.Clock, err = officialClock(req.Daily.Time)
		if err == nil {
			t.Timezone, err = canonicalZone(req.Daily.TimeZone)
		}
	case req.Weekly != nil:
		t.Kind = "weekly"
		t.Clock, err = officialClock(req.Weekly.Time)
		if err == nil {
			t.Timezone, err = canonicalZone(req.Weekly.TimeZone)
		}
		t.Weekdays = append([]int{}, req.Weekly.Weekdays...)
		if err == nil {
			err = validateWeekdays(t)
		}
		sort.Ints(t.Weekdays)
	case req.Cron != nil:
		t.Kind = "cron"
		t.Cron, err = CanonicalCron(req.Cron.Expression)
		if err == nil {
			t.Timezone, err = canonicalZone(req.Cron.TimeZone)
		}
	}
	if err != nil {
		return t, err
	}
	if t.Kind == "once" && !supportedInstant(t.At) {
		return t, inputError("time_out_of_range", "The scheduled time must use a four-digit UTC year.")
	}
	if t.Kind == "once" && !t.At.After(now) {
		return t, inputError("not_future", "The scheduled time must be strictly in the future.")
	}
	t.Next, err = Next(t, now)
	if err != nil {
		var public *InputError
		if errors.As(err, &public) {
			return t, err
		}
		return t, inputError("time_out_of_range", "The scheduled time must be representable as a four-digit-year RFC 3339 UTC instant.")
	}
	return t, nil
}

func (s *Store) createOfficial(ctx context.Context, task Task) (View, error) {
	err := s.locked(ctx, func() error {
		if err := s.checkSession(ctx, task.SessionID); err != nil {
			return err
		}
		tasks, err := s.read()
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		return s.write(append(tasks, task))
	})
	return taskView(task, time.Now()), err
}
func (s *Store) ListOfficial(ctx context.Context, sessionID string) ([]View, error) {
	out := []View{}
	err := s.locked(ctx, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		tasks, err := s.read()
		if err != nil {
			return err
		}
		now := time.Now()
		for _, t := range tasks {
			if t.SessionID == sessionID && t.Enabled {
				out = append(out, taskView(t, now))
			}
		}
		return nil
	})
	return out, err
}
func (s *Store) GetRecord(ctx context.Context, sessionID, id string) (Record, error) {
	var result Record
	err := s.locked(ctx, func() error {
		tasks, err := s.read()
		if err != nil {
			return err
		}
		for _, t := range tasks {
			if t.SessionID == sessionID && t.ID == id {
				result = taskRecord(t)
				return nil
			}
		}
		return inputError("schedule_not_found", "The reminder does not exist in this session.")
	})
	return result, err
}
func (s *Store) Catalog(ctx context.Context) ([]CatalogEntry, error) {
	out := []CatalogEntry{}
	err := s.locked(ctx, func() error {
		tasks, err := s.read()
		if err != nil {
			return err
		}
		for _, t := range tasks {
			status := "active"
			if !t.Enabled {
				status = "inactive"
			}
			out = append(out, CatalogEntry{Record: taskRecord(t), SessionID: t.SessionID, Status: status, LastDelivery: t.LastDelivery})
		}
		return nil
	})
	return out, err
}

func (s *Store) updateOfficial(ctx context.Context, sessionID, id string, expected Record, patch UpdatePatch) (UpdateResult, error) {
	result := UpdateResult{ID: id, Code: "schedule_not_found"}
	invalidatePending := false
	err := s.locked(ctx, func() error {
		if err := s.checkSession(ctx, sessionID); err != nil {
			var refusal *InputError
			if errors.As(err, &refusal) && refusal.Code == "subagent_session" {
				result.Code, result.Message = refusal.Code, refusal.Message
				return nil
			}
			return err
		}
		tasks, err := s.read()
		if err != nil {
			return err
		}
		for i := range tasks {
			current := &tasks[i]
			if current.ID != id || current.SessionID != sessionID {
				continue
			}
			if !current.Enabled {
				result.Code = "schedule_ended"
				return nil
			}
			if current.Official || current.Title != "" {
				if err := validateExpected(expected); err != nil {
					return err
				}
			}
			if !reflect.DeepEqual(taskRecord(*current), expected) {
				result.Code = "schedule_conflict"
				return nil
			}
			updated := *current
			if patch.Title != nil {
				updated.Title, err = title(*patch.Title)
				if err != nil {
					return err
				}
			}
			if patch.Prompt != nil {
				updated.Prompt, err = prompt(*patch.Prompt)
				if err != nil {
					return err
				}
			}
			if updated.Title == "" {
				return inputError("invalid_prompt", "Legacy reminders need an explicit title before official editing.")
			}
			if patch.Timing != nil {
				if patch.Timing.AfterSeconds != nil || patch.Timing.selectorCount() != 1 {
					return inputError("invalid_selector", "schedule_update accepts exactly one of at, every_seconds, daily, weekly, or cron for a timing change.")
				}
				candidate, err := prepareOfficial(sessionID, CreateRequest{Prompt: updated.Prompt, Title: updated.Title, TimingInput: *patch.Timing}, time.Now().UTC().Truncate(time.Millisecond))
				if err != nil {
					// An equivalent one-shot target is a no-op even after it became due.
					if len(patch.Timing.At) == 0 {
						return err
					}
					at, parseErr := parseAt(patch.Timing.At)
					if parseErr != nil || current.Kind != "once" || !at.Equal(current.Next) {
						return err
					}
				} else if !sameTiming(*current, candidate) {
					updated.Kind = candidate.Kind
					updated.Selector = candidate.Selector
					updated.AfterSeconds = candidate.AfterSeconds
					updated.EverySeconds = candidate.EverySeconds
					updated.At = candidate.At
					updated.Every = candidate.Every
					updated.Clock = candidate.Clock
					updated.Weekday = candidate.Weekday
					updated.Weekdays = candidate.Weekdays
					updated.Cron = candidate.Cron
					updated.Timezone = candidate.Timezone
					updated.Next = candidate.Next
					updated.CreatedAt = candidate.CreatedAt
					updated.Pending = ""
					updated.PendingAt = time.Time{}
					updated.PendingPrompt = nil
					updated.PendingNext = nil
					invalidatePending = current.Pending != ""
				}
			}
			if !current.Official {
				if err := normalizeMigration(&updated); err != nil {
					return err
				}
			}
			updated.Official = true
			if err := validateOfficialTask(updated); err != nil {
				return inputError("invalid_rule", "The edited reminder does not have a valid official timing record.")
			}
			record := taskRecord(updated)
			view := taskView(updated, time.Now())
			result = UpdateResult{ID: id, Updated: !reflect.DeepEqual(updated, *current), Record: &record, View: &view}
			if !result.Updated {
				return nil
			}
			if err = ctx.Err(); err != nil {
				return err
			}
			tasks[i] = updated
			return s.write(tasks)
		}
		return nil
	})
	if err == nil && invalidatePending {
		s.cancelActive(id)
	}
	return result, err
}

func normalizeMigration(task *Task) error {
	var err error
	task.Prompt, err = prompt(task.Prompt)
	if err != nil {
		return err
	}
	switch task.Kind {
	case "once":
		if task.Selector == "" {
			task.Selector = "at"
		}
	case "interval":
		if task.EverySeconds == 0 {
			duration, err := time.ParseDuration(task.Every)
			if err != nil || duration%time.Second != 0 {
				return inputError("invalid_rule", "A legacy fractional interval requires an explicit whole-second timing change.")
			}
			task.EverySeconds = int64(duration / time.Second)
		}
		task.Selector = "every"
	case "daily", "weekly":
		task.Clock = normalizedClock(task.Clock)
		if task.Kind == "weekly" && len(task.Weekdays) == 0 {
			day := task.Weekday
			if day == 0 {
				day = 7
			}
			task.Weekdays = []int{day}
		}
	case "cron":
		task.Cron, err = CanonicalCron(task.Cron)
		if err != nil {
			return err
		}
	}
	return nil
}
func sameTiming(a, b Task) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case "once":
		return a.Next.Equal(b.Next)
	case "interval":
		an, ae := intervalNanos(a)
		bn, be := intervalNanos(b)
		return ae == nil && be == nil && an.Cmp(bn) == 0
	case "daily":
		return normalizedClock(a.Clock) == b.Clock && a.Timezone == b.Timezone
	case "weekly":
		return normalizedClock(a.Clock) == b.Clock && a.Timezone == b.Timezone && reflect.DeepEqual(taskRecord(a).Weekdays, b.Weekdays)
	case "cron":
		canonical, err := CanonicalCron(a.Cron)
		return err == nil && canonical == b.Cron && a.Timezone == b.Timezone
	}
	return false
}

func (s *Store) deleteOfficial(ctx context.Context, sessionID, id string) (DeleteResult, error) {
	result := DeleteResult{ID: id, Code: "schedule_not_found"}
	err := s.locked(ctx, func() error {
		tasks, err := s.read()
		if err != nil {
			return err
		}
		for i, t := range tasks {
			if t.ID == id && t.SessionID == sessionID {
				if err = ctx.Err(); err != nil {
					return err
				}
				tasks = append(tasks[:i], tasks[i+1:]...)
				if err = s.write(tasks); err != nil {
					return err
				}
				result.Deleted = true
				result.Code = ""
				return nil
			}
		}
		return nil
	})
	if result.Deleted {
		s.cancelActive(id)
	}
	return result, err
}
func (s *Store) stopSession(ctx context.Context, sessionID string) error {
	var removed []string
	err := s.locked(ctx, func() error {
		tasks, err := s.read()
		if err != nil {
			return err
		}
		kept := make([]Task, 0, len(tasks))
		for _, t := range tasks {
			if t.SessionID == sessionID && t.Enabled {
				removed = append(removed, t.ID)
			} else {
				kept = append(kept, t)
			}
		}
		if len(removed) == 0 {
			return nil
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		return s.write(kept)
	})
	if err == nil {
		for _, id := range removed {
			s.cancelActive(id)
		}
	}
	return err
}
func (s *Store) cancelActive(id string) {
	s.activeMu.Lock()
	if cancel := s.active[id]; cancel != nil {
		cancel()
	}
	s.activeMu.Unlock()
}

func (s *Store) acknowledge(ctx context.Context, sessionID, id, pendingID, messageID string) error {
	if messageID == "" || strings.TrimSpace(messageID) != messageID {
		return inputError("invalid_rule", "messageId must be non-empty without surrounding whitespace.")
	}
	return s.locked(ctx, func() error {
		tasks, err := s.read()
		if err != nil {
			return err
		}
		for i := range tasks {
			current := &tasks[i]
			if current.ID != id || current.SessionID != sessionID {
				continue
			}
			if current.LastDelivery != nil && current.LastDelivery.MessageID == messageID {
				return nil
			}
			if !current.Enabled || current.Pending != pendingID || pendingID == "" {
				return inputError("schedule_conflict", "The reminder changed before its delivery acknowledgment.")
			}
			for _, saved := range current.DeliveryHistory {
				if saved.MessageID == messageID {
					return inputError("schedule_conflict", "A saved message identity cannot acknowledge another occurrence.")
				}
			}
			if err = ctx.Err(); err != nil {
				return err
			}
			now := time.Now().UTC().Truncate(time.Millisecond)
			receipt := Receipt{ScheduledAt: instant(current.PendingAt), DeliveredAt: instant(now), MessageID: messageID}
			appendReceipt(current, receipt, s.getOptions())
			advanceTask(current, now)
			return s.write(tasks)
		}
		return inputError("schedule_not_found", "The reminder does not exist in this session.")
	})
}
func appendReceipt(t *Task, receipt Receipt, options Options) {
	if !t.HistoryKnown {
		t.EarlierRecordsUnavailable = true
		t.HistoryKnown = true
		if t.LastDelivery != nil {
			t.DeliveryHistory = append(t.DeliveryHistory, DeliveryRecord{Receipt: *t.LastDelivery})
		}
	}
	for _, r := range t.DeliveryHistory {
		if r.MessageID == receipt.MessageID {
			return
		}
	}
	prompt := t.Prompt
	if t.PendingPrompt != nil {
		prompt = *t.PendingPrompt
	}
	t.DeliveryHistory = append(t.DeliveryHistory, DeliveryRecord{Receipt: receipt, Prompt: &prompt})
	t.LastDelivery = &receipt
	ack, _ := time.Parse(time.RFC3339Nano, receipt.DeliveredAt)
	cutoff := time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	if options.DeliveryHistoryDays <= 3652059 {
		cutoff = ack.AddDate(0, 0, -options.DeliveryHistoryDays)
	}
	kept := make([]DeliveryRecord, 0, len(t.DeliveryHistory))
	for i, r := range t.DeliveryHistory {
		when, err := time.Parse(time.RFC3339Nano, r.DeliveredAt)
		if i == len(t.DeliveryHistory)-1 || err == nil && !when.Before(cutoff) {
			kept = append(kept, r)
		}
	}
	if len(kept) > options.DeliveryHistoryRecords {
		kept = kept[len(kept)-options.DeliveryHistoryRecords:]
	}
	if len(kept) < len(t.DeliveryHistory) {
		t.EarlierRecordsUnavailable = true
		t.EarlierRecordsPruned = true
	}
	t.DeliveryHistory = kept
}

func validateExpected(record Record) error {
	invalid := func() error { return inputError("invalid_rule", "expected must be a complete valid Schedule record.") }
	if identity(record.ID) != nil {
		return invalid()
	}
	normalized, err := title(record.Title)
	if err != nil || normalized != record.Title {
		return invalid()
	}
	text, err := prompt(record.Prompt)
	if err != nil || text != record.Prompt {
		return invalid()
	}
	at, err := time.Parse("2006-01-02T15:04:05.000Z", record.ScheduledAt)
	if err != nil || instant(at) != record.ScheduledAt || !supportedInstant(at) {
		return invalid()
	}
	canonical := Record{ID: record.ID, Kind: record.Kind, Title: record.Title, Prompt: record.Prompt, ScheduledAt: record.ScheduledAt}
	switch record.Kind {
	case "at":
	case "after":
		if record.AfterSeconds < 1 || record.AfterSeconds > maxSafeInteger {
			return invalid()
		}
		canonical.AfterSeconds = record.AfterSeconds
	case "every":
		if record.EverySeconds < 60 || record.EverySeconds > maxSafeInteger {
			return invalid()
		}
		canonical.EverySeconds = record.EverySeconds
	case "daily", "weekly":
		clock, err := officialClock(record.Time)
		if err != nil || clock != record.Time {
			return invalid()
		}
		if _, err := canonicalZone(record.TimeZone); err != nil {
			return invalid()
		}
		canonical.Time = record.Time
		canonical.TimeZone = record.TimeZone
		if record.Kind == "weekly" {
			if err := validateWeekdays(Task{Official: true, Weekdays: record.Weekdays}); err != nil {
				return invalid()
			}
			if !sort.IntsAreSorted(record.Weekdays) {
				return invalid()
			}
			canonical.Weekdays = append([]int{}, record.Weekdays...)
		}
	case "cron":
		expression, err := CanonicalCron(record.Expression)
		if err != nil || expression != record.Expression {
			return invalid()
		}
		if _, err := canonicalZone(record.TimeZone); err != nil {
			return invalid()
		}
		canonical.Expression = record.Expression
		canonical.TimeZone = record.TimeZone
	default:
		return invalid()
	}
	if !reflect.DeepEqual(canonical, record) {
		return invalid()
	}
	return nil
}
func advanceTask(t *Task, now time.Time) {
	pendingNext := t.PendingNext
	t.LastError = ""
	t.LastDelivered = now
	t.Pending = ""
	t.PendingAt = time.Time{}
	t.PendingPrompt = nil
	t.PendingNext = nil
	if t.Kind == "once" {
		t.Enabled = false
		return
	}
	if pendingNext != nil {
		if pendingNext.IsZero() {
			t.Enabled = false
		} else {
			t.Next = pendingNext.UTC()
		}
		return
	}
	next, err := Next(*t, now)
	if err != nil {
		t.Enabled = false
		t.LastError = err.Error()
	} else {
		t.Next = next
	}
}

func (s *Store) History(ctx context.Context, sessionID, id string, limit int, before string) (HistoryPage, error) {
	options := s.getOptions()
	result := HistoryPage{ID: id, Code: "schedule_not_found", Retention: RetentionBounds{Days: options.DeliveryHistoryDays, Records: options.DeliveryHistoryRecords}}
	if limit < 1 || limit > 100 {
		return result, inputError("invalid_rule", "limit must be an integer from 1 through 100.")
	}
	err := s.locked(ctx, func() error {
		tasks, err := s.read()
		if err != nil {
			return err
		}
		for _, t := range tasks {
			if t.ID != id || t.SessionID != sessionID {
				continue
			}
			records := append([]DeliveryRecord{}, t.DeliveryHistory...)
			if !t.HistoryKnown && t.LastDelivery != nil {
				records = append(records, DeliveryRecord{Receipt: *t.LastDelivery})
			}
			end := len(records)
			if before != "" {
				found := false
				for i, r := range records {
					if r.MessageID == before {
						end = i
						found = true
						break
					}
				}
				if !found {
					result.Code = "delivery_cursor_not_found"
					return nil
				}
			}
			result.Code = ""
			result.EarlierRecordsUnavailable = t.EarlierRecordsUnavailable || !t.HistoryKnown
			result.EarlierRecordsPruned = t.EarlierRecordsPruned
			result.Records = []DeliveryRecord{}
			start := end - limit
			if start < 0 {
				start = 0
			}
			for i := end - 1; i >= start; i-- {
				result.Records = append(result.Records, records[i])
			}
			if start > 0 && len(result.Records) > 0 {
				result.NextBefore = result.Records[len(result.Records)-1].MessageID
			}
			return nil
		}
		return nil
	})
	return result, err
}

func intervalNanos(t Task) (*big.Int, error) {
	if t.EverySeconds != 0 {
		if t.EverySeconds < 60 || t.EverySeconds > maxSafeInteger {
			return nil, errors.New("invalid interval")
		}
		return new(big.Int).Mul(big.NewInt(t.EverySeconds), big.NewInt(1e9)), nil
	}
	d, err := time.ParseDuration(t.Every)
	if err != nil || d < time.Minute {
		return nil, errors.New("invalid interval")
	}
	return big.NewInt(int64(d)), nil
}
func addInterval(t time.Time, nanos *big.Int) time.Time {
	seconds, remainder := new(big.Int), new(big.Int)
	seconds.QuoRem(nanos, big.NewInt(1e9), remainder)
	return time.Unix(t.Unix()+seconds.Int64(), int64(t.Nanosecond())+remainder.Int64()).UTC()
}
func validateWeekdays(t Task) error {
	if len(t.Weekdays) == 0 {
		if t.Official {
			return inputError("invalid_rule", "weekdays must be a non-empty ISO weekday set.")
		}
		if t.Weekday < 0 || t.Weekday > 6 {
			return inputError("invalid_rule", "weekday must be 0..6")
		}
		return nil
	}
	seen := map[int]bool{}
	for _, day := range t.Weekdays {
		if day < 1 || day > 7 || seen[day] {
			return inputError("invalid_rule", "weekdays must contain unique ISO weekdays from 1 through 7.")
		}
		seen[day] = true
	}
	return nil
}
func matchesWeekday(t Task, day time.Weekday) bool {
	if len(t.Weekdays) == 0 {
		return int(day) == t.Weekday
	}
	iso := int(day)
	if iso == 0 {
		iso = 7
	}
	for _, d := range t.Weekdays {
		if d == iso {
			return true
		}
	}
	return false
}

func validateOfficialTask(t Task) error {
	if t.Official {
		normalized, err := title(t.Title)
		if err != nil || normalized != t.Title {
			return errors.New("official task has an invalid stored title")
		}
		if strings.TrimSpace(t.Prompt) != t.Prompt {
			return errors.New("official task has an untrimmed prompt")
		}
		switch t.Kind {
		case "once":
			if t.Selector != "after" && t.Selector != "at" {
				return errors.New("invalid one-shot selector")
			}
			if t.Selector == "after" && (t.AfterSeconds < 1 || t.AfterSeconds > maxSafeInteger) {
				return errors.New("invalid stored relative delay")
			}
		case "interval":
			if t.Selector != "every" || t.EverySeconds < 60 || t.EverySeconds > maxSafeInteger {
				return errors.New("invalid stored fixed interval")
			}
		case "daily", "weekly":
			clock, err := officialClock(t.Clock)
			if err != nil || clock != t.Clock {
				return errors.New("stored clock is not canonical")
			}
			if _, err := canonicalZone(t.Timezone); err != nil {
				return err
			}
		case "cron":
			expression, err := CanonicalCron(t.Cron)
			if err != nil || expression != t.Cron {
				return errors.New("stored cron is not canonical")
			}
		}
		if t.Kind == "weekly" {
			for i := 1; i < len(t.Weekdays); i++ {
				if t.Weekdays[i] <= t.Weekdays[i-1] {
					return errors.New("stored weekdays are not canonical")
				}
			}
		}
	}
	seen := map[string]bool{}
	if t.Pending == "" && t.PendingPrompt != nil {
		return errors.New("pending prompt has no pending delivery")
	}
	if t.PendingNext != nil && (t.Pending == "" || !t.PendingNext.IsZero() && !supportedInstant(*t.PendingNext)) {
		return errors.New("invalid pending next target")
	}
	for _, r := range t.DeliveryHistory {
		if err := validateReceipt(r.Receipt); err != nil {
			return err
		}
		if seen[r.MessageID] {
			return errors.New("duplicate saved delivery message identity")
		}
		seen[r.MessageID] = true
	}
	if t.LastDelivery != nil {
		if err := validateReceipt(*t.LastDelivery); err != nil {
			return err
		}
		if t.HistoryKnown && (len(t.DeliveryHistory) == 0 || t.DeliveryHistory[len(t.DeliveryHistory)-1].Receipt != *t.LastDelivery) {
			return errors.New("latest saved receipt differs from lastDelivery")
		}
	}
	if t.HistoryKnown && len(t.DeliveryHistory) > 0 && t.LastDelivery == nil {
		return errors.New("saved history has no last delivery receipt")
	}
	if t.EarlierRecordsPruned && !t.EarlierRecordsUnavailable {
		return errors.New("pruned history must mark earlier records unavailable")
	}
	return nil
}
func validateReceipt(r Receipt) error {
	if r.MessageID == "" || strings.TrimSpace(r.MessageID) != r.MessageID {
		return errors.New("invalid saved message identity")
	}
	for _, v := range []string{r.ScheduledAt, r.DeliveredAt} {
		parsed, err := time.Parse("2006-01-02T15:04:05.000Z", v)
		if err != nil || !supportedInstant(parsed) || instant(parsed) != v {
			return errors.New("invalid saved receipt timestamp")
		}
	}
	return nil
}
