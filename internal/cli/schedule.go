package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"dsgo/internal/core"
	"dsgo/internal/schedule"
)

func (a *App) configureSchedules() error {
	return a.schedules.SetOptions(schedule.Options{CheckSession: func(ctx context.Context, id string) error {
		session, err := a.store.Get(id)
		if err != nil {
			return err
		}
		if session.Delegated {
			return &schedule.InputError{Code: "delegated_session_unsupported", Message: "Reminders cannot be managed from a delegated session."}
		}
		if session.Archived {
			return &schedule.InputError{Code: "session_archived", Message: "Unarchive the session before managing reminders."}
		}
		return ctx.Err()
	}})
}

func (a *App) registerScheduleTools() error {
	for _, spec := range schedule.ToolSpecs() {
		name := spec.Name
		if err := a.registry.Register(spec, func(ctx context.Context, tc core.ToolContext, input json.RawMessage) (core.ToolResult, error) {
			session, err := a.store.Get(tc.SessionID)
			if err != nil {
				return core.ToolResult{}, err
			}
			if session.Delegated {
				return core.ToolResult{}, &schedule.InputError{Code: "delegated_session_unsupported", Message: "Reminder tools are unavailable in delegated sessions."}
			}
			if name != "schedule_list" && tc.Emit != nil {
				if err := tc.Emit("schedule/requested", map[string]any{"tool": name, "input": input}); err != nil {
					return core.ToolResult{}, err
				}
			}
			var value any
			switch name {
			case "schedule_create":
				var p schedule.CreateRequest
				p, err = schedule.DecodeCreate(input)
				if err == nil {
					value, err = a.schedules.CreateOfficial(ctx, tc.SessionID, p)
				}
			case "schedule_list":
				value, err = a.schedules.ListOfficial(ctx, tc.SessionID)
			case "schedule_delete":
				var p struct{ ID string }
				if err = json.Unmarshal(input, &p); err == nil {
					value, err = a.schedules.DeleteOfficial(ctx, tc.SessionID, p.ID)
				}
			case "schedule_update":
				var id string
				var patch schedule.UpdatePatch
				id, patch, err = schedule.DecodeUpdate(input)
				if err == nil {
					var current schedule.Record
					current, err = a.schedules.GetRecord(ctx, tc.SessionID, id)
					if err == nil {
						var result schedule.UpdateResult
						result, err = a.schedules.UpdateOfficial(ctx, tc.SessionID, id, current, patch)
						if result.Updated && result.View != nil {
							value = result.View
						} else {
							value = result
						}
					} else {
						var inputError *schedule.InputError
						if errors.As(err, &inputError) && inputError.Code == "schedule_not_found" {
							value = schedule.UpdateResult{ID: id, Code: inputError.Code}
							err = nil
						}
					}
				}
			}
			if err != nil {
				return core.ToolResult{}, err
			}
			if name != "schedule_list" && tc.Emit != nil {
				if err = tc.Emit("schedule/managed", map[string]any{"tool": name, "result": value}); err != nil {
					return core.ToolResult{}, err
				}
			}
			return toolValue(value), nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) acknowledgeDelivery(ctx context.Context, t schedule.Task) error {
	events, err := a.store.Events(t.SessionID)
	if err != nil {
		return err
	}
	for _, ev := range events {
		if ev.Type == "user/message" {
			var m core.Message
			if json.Unmarshal(ev.Data, &m) == nil && m.DeliveryID == t.Pending {
				return a.schedules.Acknowledge(ctx, t.SessionID, t.ID, t.Pending, ev.ID)
			}
		}
	}
	return fmt.Errorf("scheduled occurrence %s has no durable accepted message", t.Pending)
}
