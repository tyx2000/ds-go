package cli

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"dsgo/internal/core"
	"dsgo/internal/state"
)

// History times come from the store's clock, so the fake clock starts at the
// real time and only moves forward from there.
func timeContextApp(t *testing.T, refreshMS int64) (*App, core.Session, func(time.Duration)) {
	t.Helper()
	a, s := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		return core.Response{}, nil
	}))
	a.cfg.TimeContext = &state.TimeContextSettings{TimeZone: "Asia/Shanghai", RefreshIntervalMS: &refreshMS}
	base, offset := time.Now(), time.Duration(0)
	a.timeContext.now = func() time.Time { return base.Add(offset) }
	return a, s, func(d time.Duration) { offset += d }
}

func timeReadings(t *testing.T, a *App, id string) []string {
	t.Helper()
	messages, err := a.store.Messages(id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, message := range messages {
		if messageSourceKind(message) == "time-context" {
			out = append(out, message.Content[0].Text)
		}
	}
	return out
}

func TestTimeContextReadingsFollowOfficialFormatAndRefresh(t *testing.T) {
	a, s, advance := timeContextApp(t, 600000)
	_, _ = a.store.Append(s.ID, "turn1", "turn/started", nil)
	if err := a.prepareStep(a.ctx, s, "turn1", 1); err != nil {
		t.Fatal(err)
	}
	got := timeReadings(t, a, s.ID)
	first := regexp.MustCompile(`^Time sampled while preparing turn 1, step 1: \d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\+08:00\[Asia/Shanghai\]\n` +
		`Browser time zone for this request: Asia/Shanghai\. Interpret otherwise-unqualified dates and times in this zone\.\n` +
		`Elapsed since the preceding model-visible message: unavailable\.$`)
	if len(got) != 1 || !first.MatchString(got[0]) {
		t.Fatalf("first reading: %q", got)
	}
	// Within the 10-minute interval no reading is added.
	advance(5 * time.Minute)
	if err := a.prepareStep(a.ctx, s, "turn1", 2); err != nil {
		t.Fatal(err)
	}
	if got = timeReadings(t, a, s.ID); len(got) != 1 {
		t.Fatalf("reading inside refresh interval: %q", got)
	}
	// A later turn measures from the last model-visible message before it.
	_, _ = a.store.Append(s.ID, "turn1", "turn/finished", nil)
	advance(time.Hour + 2*time.Minute + 500*time.Millisecond)
	_, _ = a.store.Append(s.ID, "turn2", "turn/started", nil)
	if err := a.prepareStep(a.ctx, s, "turn2", 1); err != nil {
		t.Fatal(err)
	}
	got = timeReadings(t, a, s.ID)
	if len(got) != 2 || !strings.HasPrefix(got[1], "Time sampled while preparing turn 2, step 1: ") || !strings.HasSuffix(got[1], "Elapsed since the preceding model-visible message: 1h 7m 0s.") {
		t.Fatalf("second turn reading: %q", got)
	}
}

func TestTimeContextEveryStepWithZeroIntervalAndSkipsMinimal(t *testing.T) {
	a, s, advance := timeContextApp(t, 0)
	_, _ = a.store.Append(s.ID, "turn1", "turn/started", nil)
	_ = a.prepareStep(a.ctx, s, "turn1", 1)
	advance(26*time.Hour + 4*time.Second + 500*time.Millisecond)
	_ = a.prepareStep(a.ctx, s, "turn1", 2)
	got := timeReadings(t, a, s.ID)
	if len(got) != 2 || !strings.HasSuffix(got[1], "Elapsed since the preceding step context: 1d 2h 4s.") {
		t.Fatalf("every-step readings: %q", got)
	}
	s.Preset = "minimal"
	_ = a.prepareStep(a.ctx, s, "turn1", 3)
	if got = timeReadings(t, a, s.ID); len(got) != 2 {
		t.Fatalf("minimal preset received a reading: %q", got)
	}
}

func TestTimeContextWithoutKnownZoneAsksTheUser(t *testing.T) {
	a, s, _ := timeContextApp(t, 0)
	a.cfg.TimeContext.TimeZone = "Not/AZone"
	_, _ = a.store.Append(s.ID, "turn1", "turn/started", nil)
	_ = a.prepareStep(a.ctx, s, "turn1", 1)
	got := timeReadings(t, a, s.ID)
	if len(got) != 1 || !regexp.MustCompile(`\+00:00\[UTC\]\nBrowser time zone for this request: unavailable\. Ask the user to clarify`).MatchString(got[0]) {
		t.Fatalf("unknown zone reading: %q", got)
	}
}

func TestTimeContextStampAndElapsedFormats(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 29, 2, 0, 5, 0, time.UTC)
	if got := formatTimeContextStamp(at, shanghai, "Asia/Shanghai"); got != "2026-10-29T10:00:05+08:00[Asia/Shanghai]" {
		t.Fatalf("stamp %q", got)
	}
	if got := formatTimeContextStamp(at, time.UTC, "UTC"); got != "2026-10-29T02:00:05+00:00[UTC]" {
		t.Fatalf("UTC stamp %q", got)
	}
	for d, want := range map[time.Duration]string{0: "0s", -time.Second: "0s", 59 * time.Second: "59s", time.Hour: "1h 0s", 90061 * time.Second: "1d 1h 1m 1s"} {
		if got := formatElapsed(d); got != want {
			t.Fatalf("%v: %q want %q", d, got, want)
		}
	}
}
