// Portions ported from DeepSeek Harness domain.ts, Copyright (c) 2026 DeepSeek.
// MIT license: see docs/upstream/LICENSE.
package schedule

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

var cronElement = regexp.MustCompile(`^(?:\*(?:/[0-9]+)?|[0-9]+(?:-[0-9]+(?:/[0-9]+)?)?)$`)

// CanonicalCron preserves both the matched values and Vixie's leading-star
// flags. The encoding follows the pinned official domain.ts field encoder.
func CanonicalCron(expression string) (string, error) {
	if expression == "" || strings.TrimSpace(expression) != expression {
		return "", inputError("invalid_rule", "cron.expression must be a non-empty trimmed string.")
	}
	parts := strings.Fields(expression)
	if len(parts) != 5 {
		return "", inputError("invalid_rule", "cron.expression requires five fields.")
	}
	for _, part := range parts {
		for _, element := range strings.Split(part, ",") {
			if !cronElement.MatchString(element) {
				return "", inputError("invalid_rule", "cron.expression contains an unsupported field element.")
			}
			if _, step, ok := strings.Cut(element, "/"); ok {
				n, err := unsigned(step)
				if err != nil || int64(n) > maxSafeInteger {
					return "", inputError("invalid_rule", "cron field step must be a positive safe integer.")
				}
			}
		}
	}
	parsed, err := parseCron(expression)
	if err != nil {
		return "", inputError("invalid_rule", err.Error())
	}
	mins, maxs, canonicalMax := []int{0, 0, 1, 1, 0}, []int{59, 23, 31, 12, 7}, []int{59, 23, 31, 12, 6}
	for i := range parts {
		values := []int{}
		for n := range parsed.fields[i] {
			values = append(values, n)
		}
		sort.Ints(values)
		if !parsed.all[i] {
			parts[i] = encodeCronValues(values)
			continue
		}
		if len(values) == canonicalMax[i]-mins[i]+1 {
			parts[i] = "*"
			continue
		}
		bestStep := 0
		bestWalk := map[int]bool{}
		for step := 1; step <= maxs[i]-mins[i]+1; step++ {
			walk := map[int]bool{}
			subset := true
			for n := mins[i]; n <= maxs[i]; n += step {
				value := n
				if i == 4 && value == 7 {
					value = 0
				}
				walk[value] = true
				if !parsed.fields[i][value] {
					subset = false
					break
				}
			}
			if subset && len(walk) > len(bestWalk) {
				bestStep = step
				bestWalk = walk
			}
		}
		if bestStep == 0 {
			return "", inputError("invalid_rule", "cron field cannot keep its leading star.")
		}
		parts[i] = fmt.Sprintf("*/%d", bestStep)
		remaining := []int{}
		for _, value := range values {
			if !bestWalk[value] {
				remaining = append(remaining, value)
			}
		}
		if len(remaining) > 0 {
			parts[i] += "," + encodeCronValues(remaining)
		}
	}
	return strings.Join(parts, " "), nil
}

// The Gregorian date pattern repeats within 400 years. Searching calendar
// dates and eligible hours/minutes avoids enumerating 210 million UTC minutes.
func cronOccurrence(task Task, reference time.Time, backward bool) (time.Time, error) {
	parsed, err := parseCron(task.Cron)
	if err != nil {
		return time.Time{}, err
	}
	loc, err := time.LoadLocation(task.Timezone)
	if err != nil {
		return time.Time{}, err
	}
	local := reference.In(loc)
	calendar := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	direction := 1
	if backward {
		direction = -1
	}
	for days := 0; days <= 146097+366; days++ {
		day := calendar.AddDate(0, 0, direction*days)
		if !supportedInstant(day) {
			break
		}
		if backward && day.Before(task.Next.UTC().AddDate(0, 0, -2)) {
			break
		}
		if !parsed.fields[3][int(day.Month())] {
			continue
		}
		dom, dow := parsed.fields[2][day.Day()], parsed.fields[4][int(day.Weekday())]
		match := dom && dow
		if !parsed.all[2] && !parsed.all[4] {
			match = dom || dow
		}
		if !match {
			continue
		}
		for hi := 0; hi < 24; hi++ {
			hour := hi
			if backward {
				hour = 23 - hi
			}
			if !parsed.fields[1][hour] {
				continue
			}
			for mi := 0; mi < 60; mi++ {
				minute := mi
				if backward {
					minute = 59 - mi
				}
				if !parsed.fields[0][minute] {
					continue
				}
				candidate, ok := localFieldsOccurrence(day.Year(), day.Month(), day.Day(), loc, hour, minute, 0, 0)
				if !ok {
					continue
				}
				if backward {
					if !candidate.After(reference) && !candidate.Before(task.Next) {
						return candidate.UTC(), nil
					}
				} else if candidate.After(reference) && supportedInstant(candidate) {
					return candidate.UTC(), nil
				}
			}
		}
	}
	if backward {
		return task.Next, nil
	}
	return time.Time{}, inputError("invalid_rule", "cron.expression has no representable occurrence within the 400-year calendar search.")
}

func encodeCronValues(values []int) string {
	if len(values) == 1 {
		return fmt.Sprint(values[0])
	}
	step := values[1] - values[0]
	uniform := true
	for i := 2; i < len(values); i++ {
		if values[i]-values[i-1] != step {
			uniform = false
			break
		}
	}
	if uniform {
		result := fmt.Sprintf("%d-%d", values[0], values[len(values)-1])
		if step > 1 {
			result += fmt.Sprintf("/%d", step)
		}
		return result
	}
	parts := []string{}
	first, previous := values[0], values[0]
	appendRun := func() {
		if first == previous {
			parts = append(parts, fmt.Sprint(first))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", first, previous))
		}
	}
	for _, value := range values[1:] {
		if value != previous+1 {
			appendRun()
			first = value
		}
		previous = value
	}
	appendRun()
	return strings.Join(parts, ",")
}
