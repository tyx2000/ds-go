package cli

import (
	"encoding/json"
	"errors"
	"strings"

	"dsgo/internal/agent"
	"dsgo/internal/state"
)

func (a *App) goalCommand(arg string) error {
	service := a.goalService()
	if service == nil {
		return errors.New("goal storage is unavailable")
	}
	id := a.session.ID
	view, err := service.Get(id)
	if err != nil {
		return err
	}
	arg = strings.TrimSpace(arg)
	if arg == "" || arg == "status" {
		value := goalValue(view)
		if view == nil {
			legacy, err := service.LegacyObjective(id)
			if err != nil {
				return err
			}
			if legacy != "" {
				a.print(map[string]any{"goal": nil, "legacy_objective": legacy, "activation": "disarmed"})
				return nil
			}
		}
		a.print(json.RawMessage(value.Content))
		return nil
	}
	action, tail, _ := strings.Cut(arg, " ")
	if action == "pause" || action == "resume" || action == "clear" || action == "complete" || action == "edit" {
		if view == nil {
			return goalFailure("GOAL_NOT_FOUND", "no current goal; create one with /goal OBJECTIVE")
		}
		edit := state.GoalEdit{}
		if action == "edit" {
			args, err := decodeGoalArguments([]byte(tail))
			if err != nil {
				return err
			}
			edit.Objective = args.Objective
			if args.MaxGoalRounds != nil {
				cap, err := goalInteger(*args.MaxGoalRounds, false)
				if err != nil {
					return err
				}
				edit.MaxGoalRounds = &cap
			}
		} else if strings.TrimSpace(tail) != "" {
			return errors.New("goal action takes no extra arguments")
		}
		mutation, err := service.Update(id, "", view.GoalRef, action, edit, nil)
		if err != nil {
			return err
		}
		a.notice(agent.Notice{SessionID: id, Type: "event", Data: mutation.Event})
		if action == "pause" && a.engine != nil {
			a.engine.Cancel(id)
		}
		return nil
	}
	objective := arg
	cap, _ := a.goalDefaults()
	if action == "create" {
		objective = strings.TrimSpace(tail)
	}
	if strings.HasPrefix(objective, "{") {
		args, err := decodeGoalArguments([]byte(objective))
		if err != nil {
			return err
		}
		if args.Objective == nil {
			return errors.New("goal requires objective")
		}
		objective = *args.Objective
		if args.MaxGoalRounds != nil {
			cap, err = goalInteger(*args.MaxGoalRounds, false)
			if err != nil {
				return err
			}
		}
	}
	mutation, err := service.Create(id, "", objective, cap)
	if err != nil {
		return err
	}
	a.notice(agent.Notice{SessionID: id, Type: "event", Data: mutation.Event})
	return nil
}
