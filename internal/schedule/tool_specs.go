// Tool contracts ported from DeepSeek Harness, Copyright (c) 2026 DeepSeek.
// MIT license: see docs/upstream/LICENSE.
package schedule

import "dsgo/internal/core"

// ToolSpecs ports the four official dsh-tool-schedule input schemas. Session
// binding and the delegated-agent guard belong to the calling tool executor.
func ToolSpecs() []core.ToolSpec {
	properties := map[string]any{
		"every_seconds": map[string]any{"type": "number", "description": "Fixed-rate interval in whole seconds, at least 60, aligned to the creation time; changing it with schedule_update re-aligns it to the save time."},
		"daily":         map[string]any{"type": "object", "additionalProperties": false, "description": "Every day at a local time.", "properties": map[string]any{"time": map[string]any{"type": "string", "description": "HH:mm:ss with optional 1-3 fractional digits, for example 23:00:00."}, "time_zone": map[string]any{"type": "string", "description": "UTC or IANA Area/Location, for example Asia/Shanghai."}}, "required": []string{"time", "time_zone"}},
		"weekly":        map[string]any{"type": "object", "additionalProperties": false, "description": "On the given weekdays at a local time.", "properties": map[string]any{"time": map[string]any{"type": "string", "description": "HH:mm:ss with optional 1-3 fractional digits, for example 09:00:00."}, "time_zone": map[string]any{"type": "string", "description": "UTC or IANA Area/Location, for example Asia/Shanghai."}, "weekdays": map[string]any{"type": "array", "description": "ISO weekdays, Monday 1 through Sunday 7, without repetitions.", "items": map[string]string{"type": "integer"}}}, "required": []string{"time", "time_zone", "weekdays"}},
		"cron":          map[string]any{"type": "object", "additionalProperties": false, "description": "Five-field Vixie cron expression in a time zone.", "properties": map[string]any{"expression": map[string]any{"type": "string", "description": "minute hour day-of-month month day-of-week, for example \"*/15 9-17 * * 1-5\". When both day fields are restricted, a date matches if either one matches."}, "time_zone": map[string]any{"type": "string", "description": "UTC or IANA Area/Location, for example Asia/Shanghai."}}, "required": []string{"expression", "time_zone"}},
		"at":            map[string]any{"description": "Absolute target: an RFC 3339 date-time with offset, or a local date, time, and IANA time_zone.", "oneOf": []any{map[string]string{"type": "string"}, map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"date": map[string]string{"type": "string"}, "time": map[string]string{"type": "string"}, "time_zone": map[string]string{"type": "string"}}, "required": []string{"date", "time", "time_zone"}}}},
	}
	clone := func() map[string]any {
		out := map[string]any{}
		for k, v := range properties {
			out[k] = v
		}
		return out
	}
	create := clone()
	create["prompt"] = map[string]any{"type": "string", "description": "Reminder content to present when the target becomes due."}
	create["title"] = map[string]any{"type": "string", "description": "Concise task name in the user's language, at most 120 characters. Name only the core action and subject; keep essential target identifiers such as PR numbers and branch names. Do not include execution frequency, scheduled dates/times, time zones, task status, or configuration and execution details. Do not append parenthetical notes. Put scheduling information in the timing parameters and execution instructions in prompt."}
	create["after_seconds"] = map[string]any{"type": "number", "description": "Delay in whole seconds."}
	update := clone()
	update["id"] = map[string]any{"type": "string", "description": "Schedule id returned by schedule_list."}
	update["title"] = map[string]any{"type": "string", "description": "New task name; follow the schedule_create title rules."}
	update["prompt"] = map[string]any{"type": "string", "description": "New reminder content."}
	schema := func(props map[string]any, required ...string) []byte {
		value := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(required) > 0 {
			value["required"] = required
		}
		return core.JSON(value)
	}
	return []core.ToolSpec{
		{Name: "schedule_create", Description: "Create a reminder in the current session that delivers prompt when it becomes due. Supply exactly one timing parameter: after_seconds, at, every_seconds, daily, weekly, or cron. Local times that do not exist in the zone are skipped; repeated local times fire once, at the earlier instant. After downtime, a recurring reminder delivers only its latest missed occurrence. Delivery can repeat after a crash.", InputSchema: schema(create, "prompt", "title")},
		{Name: "schedule_list", Description: "List the active reminders in the current session.", InputSchema: schema(map[string]any{})},
		{Name: "schedule_delete", Description: "Delete a reminder in the current session, active or inactive. Deletion does not retract a reminder message that is already queued.", InputSchema: schema(map[string]any{"id": map[string]any{"type": "string", "description": "Exact schedule id."}}, "id")},
		{Name: "schedule_update", Description: "Change a reminder in place, keeping its id. Supply a new title, prompt, or at most one timing parameter; omitted fields keep their stored values. To change a relative delay, create a new reminder.", InputSchema: schema(update, "id")},
	}
}
