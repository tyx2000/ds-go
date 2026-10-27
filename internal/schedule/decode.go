package schedule

import (
	"bytes"
	"encoding/json"
	"math"
)

var timingKeys = map[string]bool{"after_seconds": true, "at": true, "every_seconds": true, "daily": true, "weekly": true, "cron": true}

// DecodeCreate validates the official tool's exact keys and JavaScript safe
// integer semantics, including JSON spellings such as 60.0 or 6e1.
func DecodeCreate(raw json.RawMessage) (CreateRequest, error) {
	var request CreateRequest
	object, err := objectValue(raw)
	if err != nil {
		return request, err
	}
	count := 0
	for key := range object {
		if key == "prompt" || key == "title" {
			continue
		}
		if !timingKeys[key] {
			return request, inputError("invalid_selector", "schedule_create contains an unsupported parameter.")
		}
		count++
	}
	if count != 1 {
		return request, inputError("invalid_selector", "schedule_create accepts exactly one of after_seconds, at, every_seconds, daily, weekly, or cron.")
	}
	request.Prompt, err = stringValue(object["prompt"], "invalid_prompt", "prompt must be a string.")
	if err != nil {
		return request, err
	}
	if _, err = prompt(request.Prompt); err != nil {
		return request, err
	}
	request.Title, err = stringValue(object["title"], "invalid_prompt", "title is required and must be a string.")
	if err != nil {
		return request, err
	}
	if _, err = title(request.Title); err != nil {
		return request, err
	}
	request.TimingInput, err = decodeTiming(object)
	return request, err
}

// DecodeUpdate supplies the model consumer's patch. The consumer captures a
// current complete Record separately and passes it to UpdateOfficial for CAS.
func DecodeUpdate(raw json.RawMessage) (id string, patch UpdatePatch, err error) {
	object, err := objectValue(raw)
	if err != nil {
		return "", patch, err
	}
	count := 0
	for key := range object {
		if key == "id" || key == "prompt" || key == "title" {
			continue
		}
		if !timingKeys[key] || key == "after_seconds" {
			return "", patch, inputError("invalid_selector", "schedule_update accepts at most one of at, every_seconds, daily, weekly, or cron.")
		}
		count++
	}
	if count > 1 {
		return "", patch, inputError("invalid_selector", "schedule_update accepts at most one of at, every_seconds, daily, weekly, or cron.")
	}
	id, err = stringValue(object["id"], "invalid_rule", "schedule_update id must be non-empty without surrounding whitespace.")
	if err != nil {
		return
	}
	if err = identity(id); err != nil {
		return
	}
	if value, present := object["title"]; present {
		var text string
		text, err = stringValue(value, "invalid_prompt", "title must be a string.")
		if err != nil {
			return
		}
		if _, err = title(text); err != nil {
			return
		}
		patch.Title = &text
	}
	if value, present := object["prompt"]; present {
		var text string
		text, err = stringValue(value, "invalid_prompt", "prompt must be a string.")
		if err != nil {
			return
		}
		if _, err = prompt(text); err != nil {
			return
		}
		patch.Prompt = &text
	}
	if count == 0 && patch.Title == nil && patch.Prompt == nil {
		return "", patch, inputError("invalid_selector", "schedule_update needs a new title, prompt, or timing selector.")
	}
	if count == 1 {
		var timing TimingInput
		timing, err = decodeTiming(object)
		patch.Timing = &timing
	}
	return
}

func objectValue(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, inputError("invalid_rule", "Schedule arguments must be an object.")
	}
	return object, nil
}
func stringValue(raw json.RawMessage, code, message string) (string, error) {
	var text string
	if len(raw) == 0 || raw[0] != '"' {
		return "", inputError(code, message)
	}
	if err := json.Unmarshal(raw, &text); err != nil {
		return "", inputError(code, message)
	}
	return text, nil
}
func safeInteger(raw json.RawMessage) (int64, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return 0, inputError("invalid_rule", "Timing values must be safe integers.")
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, inputError("invalid_rule", "Timing values must be safe integers.")
	}
	numeric, err := number.Float64()
	if err != nil || math.IsInf(numeric, 0) || math.IsNaN(numeric) || math.Trunc(numeric) != numeric || numeric > float64(maxSafeInteger) || numeric < float64(-maxSafeInteger) {
		return 0, inputError("invalid_rule", "Timing values must be safe integers.")
	}
	return int64(numeric), nil
}

func decodeTiming(object map[string]json.RawMessage) (TimingInput, error) {
	var timing TimingInput
	if raw, ok := object["after_seconds"]; ok {
		value, err := safeInteger(raw)
		if err != nil {
			return timing, err
		}
		timing.AfterSeconds = &value
	}
	if raw, ok := object["every_seconds"]; ok {
		value, err := safeInteger(raw)
		if err != nil {
			return timing, err
		}
		timing.EverySeconds = &value
	}
	if raw, ok := object["at"]; ok {
		timing.At = append(json.RawMessage{}, raw...)
	}
	for _, kind := range []string{"daily", "weekly", "cron"} {
		raw, present := object[kind]
		if !present {
			continue
		}
		selector, err := objectValue(raw)
		if err != nil {
			return timing, err
		}
		allowed := map[string]bool{"time": true, "time_zone": true}
		if kind == "weekly" {
			allowed["weekdays"] = true
		}
		if kind == "cron" {
			delete(allowed, "time")
			allowed["expression"] = true
		}
		for key := range selector {
			if !allowed[key] {
				return timing, inputError("invalid_rule", "Timing selector contains an unsupported field.")
			}
		}
		for key := range allowed {
			if _, ok := selector[key]; !ok {
				return timing, inputError("invalid_rule", "Timing selector is missing a required field.")
			}
		}
		zone, err := stringValue(selector["time_zone"], "invalid_time_zone", "time_zone must be a string.")
		if err != nil {
			return timing, err
		}
		if kind == "cron" {
			expression, err := stringValue(selector["expression"], "invalid_rule", "cron.expression must be a string.")
			if err != nil {
				return timing, err
			}
			timing.Cron = &CronInput{Expression: expression, TimeZone: zone}
			continue
		}
		clock, err := stringValue(selector["time"], "invalid_rule", "time must be a string.")
		if err != nil {
			return timing, err
		}
		if kind == "daily" {
			timing.Daily = &DailyInput{Time: clock, TimeZone: zone}
			continue
		}
		var rawDays []json.RawMessage
		if err = json.Unmarshal(selector["weekdays"], &rawDays); err != nil || len(rawDays) == 0 {
			return timing, inputError("invalid_rule", "weekdays must be a non-empty ISO weekday set.")
		}
		days := make([]int, 0, len(rawDays))
		for _, raw := range rawDays {
			day, err := safeInteger(raw)
			if err != nil || day < 1 || day > 7 {
				return timing, inputError("invalid_rule", "weekdays must contain ISO weekdays from 1 through 7.")
			}
			days = append(days, int(day))
		}
		timing.Weekly = &WeeklyInput{Time: clock, TimeZone: zone, Weekdays: days}
	}
	return timing, nil
}
