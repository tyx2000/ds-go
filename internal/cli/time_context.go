package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"dsgo/internal/agent"
	"dsgo/internal/core"
	"dsgo/internal/timezone"
)

// The standard, ptc and cordis presets carry the official time-context
// reading (packages/context/time-context): on eligible steps a durable user
// message gives the current time, the request's time zone and the elapsed
// time since the preceding model-visible message.
const timeContextRefreshMS = 600000

// The browser's zone in the Web version is the client's zone; a terminal's
// client is this machine, so its configured or local zone takes that role.
type timeContextRuntime struct {
	mu       sync.Mutex
	resolved bool
	zone     string // empty when no zone could be determined
	location *time.Location
	now      func() time.Time
}

func (a *App) timeContextZone() (string, *time.Location) {
	r := &a.timeContext
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.resolved {
		name := ""
		if a.cfg.TimeContext != nil {
			name = a.cfg.TimeContext.TimeZone
		}
		if name == "" {
			name = localZoneName()
		}
		if canonical, location, err := timezone.Resolve(name); err == nil && name != "" {
			r.zone, r.location = canonical, location
		}
		r.resolved = true
	}
	return r.zone, r.location
}

// localZoneName finds the IANA name behind time.Local, which Go reports as "Local".
func localZoneName() string {
	if name := strings.TrimPrefix(os.Getenv("TZ"), ":"); name != "" {
		return name
	}
	target, err := os.Readlink("/etc/localtime")
	if err != nil {
		return ""
	}
	if _, name, ok := strings.Cut(target, "zoneinfo/"); ok {
		return name
	}
	return ""
}

func (a *App) timeContextNow() time.Time {
	a.timeContext.mu.Lock()
	now := a.timeContext.now
	a.timeContext.mu.Unlock()
	if now != nil {
		return now()
	}
	return time.Now()
}

// appendTimeContext records a reading before a model step, folding its
// baselines from durable history so restarts and forks keep the same state.
func (a *App) appendTimeContext(ctx context.Context, s core.Session, turn string, step int) error {
	if sessionPreset(s) == "minimal" {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	refresh := int64(timeContextRefreshMS)
	if a.cfg.TimeContext != nil && a.cfg.TimeContext.RefreshIntervalMS != nil {
		refresh = *a.cfg.TimeContext.RefreshIntervalMS
	}
	events, err := a.store.Events(s.ID)
	if err != nil {
		return err
	}
	var lastMessage, lastInjection, lastTurnInjection, beforeTurn *time.Time
	ordinal, turnOrdinal := 0, 0
	for _, event := range events {
		at := event.Time
		switch event.Type {
		case "turn/started":
			ordinal++
			if event.TurnID == turn && turnOrdinal == 0 {
				turnOrdinal, beforeTurn = ordinal, lastMessage
			}
			lastTurnInjection = nil
		case "turn/finished":
			lastTurnInjection = nil
		case "user/message":
			lastMessage = &at
			var message core.Message
			if json.Unmarshal(event.Data, &message) == nil && messageSourceKind(message) == "time-context" {
				lastInjection, lastTurnInjection = &at, &at
			}
		case "assistant/message", "tool/result":
			lastMessage = &at
		}
	}
	if turnOrdinal == 0 {
		turnOrdinal = ordinal + 1
	}
	now := a.timeContextNow()
	if refresh > 0 && lastInjection != nil && !now.Before(*lastInjection) && now.Sub(*lastInjection) < time.Duration(refresh)*time.Millisecond {
		return nil
	}
	// Step 1 measures from before this turn's input; later steps from this
	// turn's previous reading.
	previous, baseline := beforeTurn, "model-visible message"
	if step > 1 {
		previous, baseline = lastTurnInjection, "step context"
	}
	elapsed := "unavailable"
	if previous != nil {
		elapsed = formatElapsed(now.Sub(*previous))
	}
	zone, location := a.timeContextZone()
	zoneLine := "Browser time zone for this request: unavailable. Ask the user to clarify otherwise-unqualified dates and times."
	stampZone := "UTC"
	if zone != "" {
		zoneLine = "Browser time zone for this request: " + zone + ". Interpret otherwise-unqualified dates and times in this zone."
		stampZone = zone
	} else {
		location = time.UTC
	}
	text := fmt.Sprintf("Time sampled while preparing turn %d, step %d: %s\n%s\nElapsed since the preceding %s: %s.", turnOrdinal, step, formatTimeContextStamp(now, location, stampZone), zoneLine, baseline, elapsed)
	message := core.TextMessage("user", text)
	message.Source = map[string]any{"kind": "time-context", "form": "snapshot", "sections": []map[string]string{{"name": "time-context", "text": text}}}
	event, err := a.store.Append(s.ID, turn, "user/message", message)
	if err != nil {
		return err
	}
	a.notice(agent.Notice{SessionID: s.ID, Type: "event", Data: event})
	return nil
}

func messageSourceKind(message core.Message) string {
	if source, ok := message.Source.(map[string]any); ok {
		kind, _ := source["kind"].(string)
		return kind
	}
	return ""
}

// formatTimeContextStamp matches the official ISO-shaped timestamp with a
// numeric offset and the zone label, e.g. 2026-10-29T10:00:00+08:00[Asia/Shanghai].
func formatTimeContextStamp(now time.Time, location *time.Location, zone string) string {
	return now.In(location).Format("2006-01-02T15:04:05-07:00") + "[" + zone + "]"
}

// formatElapsed writes whole-second durations as 1d 2h 3m 4s, omitting
// leading zero units.
func formatElapsed(d time.Duration) string {
	seconds := int64(max(d, 0) / time.Second)
	days, hours, minutes := seconds/86400, seconds%86400/3600, seconds%3600/60
	parts := []string{}
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	return strings.Join(append(parts, fmt.Sprintf("%ds", seconds%60)), " ")
}
