package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"dsgo/internal/core"
	"dsgo/internal/schedule"
)

func (a *App) configureSchedules() error {
	return a.schedules.SetOptions(schedule.Options{DeliveryHistoryDays: a.cfg.ScheduleDeliveryHistoryDays, DeliveryHistoryRecords: a.cfg.ScheduleDeliveryHistoryRecords, CheckSession: func(ctx context.Context, id string) error {
		session, err := a.store.Get(id)
		if err != nil {
			return err
		}
		depth, err := a.subagentDepth(id)
		if err != nil {
			return err
		}
		if depth > 0 {
			return &schedule.InputError{Code: "subagent_session", Message: "This Session belongs to subagent routing, which never receives reminder delivery."}
		}
		if session.Archived {
			return &schedule.InputError{Code: "session_archived", Message: "Unarchive the session before managing reminders."}
		}
		return ctx.Err()
	}, Warn: func(err error) { a.line("Schedule: %s\n", clean(err.Error())) }})
}

func (a *App) registerScheduleTools() error {
	for _, spec := range schedule.ToolSpecs() {
		name := spec.Name
		if err := a.registry.Register(spec, func(ctx context.Context, tc core.ToolContext, input json.RawMessage) (core.ToolResult, error) {
			_, err := a.store.Get(tc.SessionID)
			if err != nil {
				return scheduleFailure(err), nil
			}
			depth, err := a.subagentDepth(tc.SessionID)
			if err != nil {
				return scheduleFailure(err), nil
			}
			if depth > 0 {
				return toolValue(map[string]any{"code": "subagent_session", "message": "A delegated subagent cannot use reminders."}), nil
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
					if p.ID == "" || strings.TrimSpace(p.ID) != p.ID {
						err = &schedule.InputError{Code: "invalid_rule", Message: "schedule_delete id must be non-empty without surrounding whitespace."}
					} else {
						value, err = a.schedules.DeleteOfficial(ctx, tc.SessionID, p.ID)
					}
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
				return scheduleFailure(err), nil
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

func scheduleFailure(err error) core.ToolResult {
	var input *schedule.InputError
	if errors.As(err, &input) {
		return toolValue(map[string]any{"code": input.Code, "message": input.Message})
	}
	return toolValue(map[string]any{"code": "internal_error", "message": "The schedule operation failed."})
}

func isScheduleMessage(message core.Message) bool {
	var source struct {
		Kind string `json:"kind"`
	}
	data, _ := json.Marshal(message.Source)
	_ = json.Unmarshal(data, &source)
	if source.Kind == "schedule" {
		return true
	}
	// The original Go daemon had no Source but used a reserved random 20-hex
	// pending id and this exact UTC framing. Ordinary input:/inbox: identities
	// cannot become scheduled work merely by resembling the reminder wording.
	if len(message.DeliveryID) != 20 || strings.ToLower(message.DeliveryID) != message.DeliveryID || len(message.Content) != 1 || message.Content[0].Type != "text" {
		return false
	}
	if _, err := hex.DecodeString(message.DeliveryID); err != nil {
		return false
	}
	line, body, ok := strings.Cut(message.Content[0].Text, "\n")
	if !ok || strings.TrimSpace(body) == "" || !strings.HasPrefix(line, "Scheduled reminder (") || !strings.HasSuffix(line, "):") {
		return false
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(line, "Scheduled reminder ("), "):")
	at, err := time.Parse(time.RFC3339, raw)
	return err == nil && strings.HasSuffix(raw, "Z") && at.UTC().Format(time.RFC3339) == raw
}

func (a *App) scheduleBatchReceipt(sessionID, deliveryID string) (string, error) {
	events, err := a.store.Events(sessionID)
	if err != nil {
		return "", err
	}
	for _, event := range events {
		if event.Type == "user/inbox" {
			var entry struct {
				Message core.Message `json:"message"`
			}
			if err := json.Unmarshal(event.Data, &entry); err != nil {
				return "", err
			}
			if entry.Message.DeliveryID == deliveryID {
				return event.ID, nil
			}
		}
		if messages, recognized, err := core.UserInputMessages(event); recognized {
			if err != nil {
				return "", err
			}
			if messages[0].DeliveryID == deliveryID {
				return event.ID, nil
			}
		}
	}
	return "", nil
}

func (a *App) wakeScheduled(ctx context.Context, sessionID string) {
	a.background.Add(1)
	go func() {
		defer a.background.Done()
		if err := a.engine.ContinueInbox(ctx, sessionID); err != nil && !errors.Is(err, context.Canceled) {
			a.line("Scheduled session %s stopped: %s\n", sessionID, clean(err.Error()))
		}
	}()
}

func (a *App) admitScheduleBatch(admissionCtx, lifetime context.Context, batch schedule.DeliveryBatch) error {
	session, err := a.store.Get(batch.SessionID)
	if err != nil {
		return err
	}
	if session.Archived {
		return errors.New("target session is archived")
	}
	depth, err := a.subagentDepth(session.ID)
	if err != nil {
		return err
	}
	if depth > 0 {
		return errors.New("a delegated session cannot receive reminders")
	}
	messageID, err := a.scheduleBatchReceipt(batch.SessionID, batch.DeliveryID)
	if err != nil {
		return err
	}
	if messageID == "" {
		batch, err = a.schedules.RefreshBatch(admissionCtx, batch)
		if err != nil {
			return err
		}
		if len(batch.Tasks) == 0 {
			return schedule.ErrNotDue
		}
		input := core.TextMessage("user", batch.Text())
		input.Source = map[string]any{"kind": "schedule"}
		input.DeliveryID = batch.DeliveryID
		if _, err = a.engine.Inbox(admissionCtx, batch.SessionID, input); err != nil {
			return err
		}
		messageID, err = a.scheduleBatchReceipt(batch.SessionID, batch.DeliveryID)
		if err != nil {
			return err
		}
		if messageID == "" {
			return errors.New("reminder inbox did not acknowledge persistence")
		}
	}
	a.wakeScheduled(lifetime, batch.SessionID)
	return a.schedules.AcknowledgeBatch(admissionCtx, batch, messageID)
}

func (a *App) acknowledgeDelivery(ctx context.Context, t schedule.Task) error {
	events, err := a.store.Events(t.SessionID)
	if err != nil {
		return err
	}
	for _, ev := range events {
		if messages, recognized, decodeErr := core.UserInputMessages(ev); recognized {
			if decodeErr != nil {
				return decodeErr
			}
			if messages[0].DeliveryID == t.Pending {
				return a.schedules.Acknowledge(ctx, t.SessionID, t.ID, t.Pending, ev.ID)
			}
		}
	}
	return fmt.Errorf("scheduled occurrence %s has no durable accepted message", t.Pending)
}
