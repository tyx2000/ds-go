package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"dsgo/internal/agent"
	"dsgo/internal/core"
	"dsgo/internal/state"
)

func goalToolSpecs() []core.ToolSpec {
	return []core.ToolSpec{
		toolSpec("get_goal", "Read the current session goal, including the id and revision that update_goal requires.", `{"type":"object","properties":{},"additionalProperties":false}`),
		toolSpec("create_goal", "Create a persisted goal that keeps this session working across automatic continuation rounds. Use it when the direct human request is a long-running objective, even if the user did not say goal; not for single-turn work.", `{"type":"object","additionalProperties":false,"required":["objective"],"properties":{"objective":{"type":"string"},"max_goal_rounds":{"type":"number"}}}`),
		toolSpec("update_goal", "Update the current goal. Read get_goal first and use the exact id and revision.", `{"type":"object","additionalProperties":false,"required":["goal_id","revision","action"],"properties":{"goal_id":{"type":"string"},"revision":{"type":"number"},"action":{"type":"string","enum":["edit","pause","resume","complete","blocked"]},"objective":{"type":"string"},"max_goal_rounds":{"type":"number"},"blocked_reason":{"type":"string"}}}`),
	}
}
func (a *App) registerGoalTools() error {
	handlers := []func(context.Context, core.ToolContext, json.RawMessage) (core.ToolResult, error){a.getGoal, a.createGoal, a.updateGoal}
	for i, spec := range goalToolSpecs() {
		if err := a.registry.Register(spec, handlers[i]); err != nil {
			return err
		}
	}
	return nil
}

type goalAuthority struct {
	human bool
	round *state.GoalView
}

func directGoalHuman(message core.Message) bool {
	if message.Source == nil {
		return true
	}
	var source struct{ Kind string }
	return json.Unmarshal(core.JSON(message.Source), &source) == nil && source.Kind == "user"
}
func (a *App) goalAuthority(tc core.ToolContext) (goalAuthority, error) {
	if a.engine == nil || !a.engine.ToolExecutionLive(tc) {
		return goalAuthority{}, goalFailure("GOAL_TOOL_DRIVER_REQUIRED", "goal tools require the exact live calling execution inside an open turn")
	}
	events, err := a.store.Events(tc.SessionID)
	if err != nil {
		return goalAuthority{}, err
	}
	view, err := a.goalService().Get(tc.SessionID)
	if err != nil {
		return goalAuthority{}, err
	}
	// The official roots() relation follows the live runtime owner, not the
	// selected conversation or the durable session's fork/delegated ancestry.
	root := a.goalRuntimeRoot(tc.SessionID)
	result := goalAuthority{}
	open := false
	for _, event := range events {
		if event.TurnID != tc.TurnID {
			continue
		}
		if event.Type == "turn/started" {
			open = true
		}
		if event.Type == "turn/finished" {
			open = false
		}
		messages, ok, err := core.UserInputMessages(event)
		if err != nil {
			return goalAuthority{}, err
		}
		if !ok {
			continue
		}
		message := messages[0]
		if root && directGoalHuman(message) {
			result.human = true
		}
		source, isRound, err := state.GoalSource(message)
		if err != nil {
			return goalAuthority{}, err
		}
		if isRound && view != nil && source.GoalID == view.ID && source.Revision == view.Revision && source.Round == view.RoundsStarted {
			result.round = view
		}
	}
	if !open {
		return goalAuthority{}, goalFailure("GOAL_TOOL_DRIVER_REQUIRED", "goal tools require an open model turn")
	}
	return result, nil
}
func (a *App) getGoal(ctx context.Context, tc core.ToolContext, _ json.RawMessage) (core.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return core.ToolResult{}, err
	}
	if _, err := a.goalAuthority(tc); err != nil {
		return core.ToolResult{}, err
	}
	view, err := a.goalService().Get(tc.SessionID)
	return goalValue(view), err
}

type goalArguments struct {
	Objective     *string      `json:"objective"`
	MaxGoalRounds *json.Number `json:"max_goal_rounds"`
	GoalID        string       `json:"goal_id"`
	Revision      json.Number  `json:"revision"`
	Action        string       `json:"action"`
	BlockedReason *string      `json:"blocked_reason"`
}

func decodeGoalArguments(raw json.RawMessage) (goalArguments, error) {
	var args goalArguments
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return args, err
	}
	return args, nil
}
func goalInteger(number json.Number, zeroOmitted bool) (int64, error) {
	if number == "" {
		if zeroOmitted {
			return 0, nil
		}
		return 0, goalFailure("GOAL_TOOL_INVALID_UPDATE", "a positive safe integer is required")
	}
	rational, ok := new(big.Rat).SetString(string(number))
	if !ok || !rational.IsInt() || !rational.Num().IsInt64() {
		return 0, goalFailure("GOAL_TOOL_INVALID_UPDATE", "a positive safe integer is required")
	}
	value := rational.Num().Int64()
	if value == 0 && zeroOmitted {
		return 0, nil
	}
	if value < 1 || value > state.MaxGoalInteger {
		return 0, goalFailure("GOAL_TOOL_INVALID_UPDATE", "a positive safe integer is required")
	}
	return value, nil
}
func (a *App) createGoal(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	authority, err := a.goalAuthority(tc)
	if err != nil {
		return core.ToolResult{}, err
	}
	if !authority.human {
		return core.ToolResult{}, goalFailure("GOAL_TOOL_AUTHORITY_REQUIRED", "creating a goal requires a direct human turn on a top-level agent")
	}
	args, err := decodeGoalArguments(raw)
	if err != nil {
		return core.ToolResult{}, err
	}
	if args.Objective == nil {
		return core.ToolResult{}, goalFailure("GOAL_INVALID_OBJECTIVE", "objective is required")
	}
	cap, _ := a.goalDefaults()
	if args.MaxGoalRounds != nil {
		cap, err = goalInteger(*args.MaxGoalRounds, false)
		if err != nil {
			return core.ToolResult{}, err
		}
	}
	if err = ctx.Err(); err != nil {
		return core.ToolResult{}, err
	}
	mutation, err := a.goalService().Create(tc.SessionID, tc.TurnID, *args.Objective, cap)
	if err != nil {
		return core.ToolResult{}, err
	}
	a.notice(agent.Notice{SessionID: tc.SessionID, Type: "event", Data: mutation.Event})
	return goalValue(mutation.View), nil
}
func (a *App) updateGoal(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	authority, err := a.goalAuthority(tc)
	if err != nil {
		return core.ToolResult{}, err
	}
	args, err := decodeGoalArguments(raw)
	if err != nil {
		return core.ToolResult{}, err
	}
	revision, err := goalInteger(args.Revision, false)
	if err != nil {
		return core.ToolResult{}, err
	}
	if args.GoalID == "" || strings.TrimSpace(args.GoalID) != args.GoalID {
		return core.ToolResult{}, goalFailure("GOAL_TOOL_INVALID_UPDATE", "goal_id must be non-empty and normalized")
	}
	ref := state.GoalRef{ID: args.GoalID, Revision: revision}
	hasObjective := args.Objective != nil && *args.Objective != ""
	hasReason := args.BlockedReason != nil && *args.BlockedReason != ""
	var cap int64
	if args.MaxGoalRounds != nil {
		cap, err = goalInteger(*args.MaxGoalRounds, true)
		if err != nil {
			return core.ToolResult{}, err
		}
	}
	edit := state.GoalEdit{}
	if hasObjective {
		edit.Objective = args.Objective
	}
	if cap != 0 {
		edit.MaxGoalRounds = &cap
	}
	operation := args.Action
	var reason *state.GoalBlockReason
	switch operation {
	case "edit", "pause", "resume":
		if !authority.human {
			return core.ToolResult{}, goalFailure("GOAL_TOOL_AUTHORITY_REQUIRED", "this goal operation requires a direct human turn on a top-level agent")
		}
		if hasReason || operation != "edit" && (hasObjective || cap != 0) {
			return core.ToolResult{}, goalFailure("GOAL_TOOL_INVALID_UPDATE", "replacement fields belong only to edit; blocked_reason belongs only to blocked")
		}
		if operation == "resume" {
			view, err := a.goalService().Get(tc.SessionID)
			if err != nil {
				return core.ToolResult{}, err
			}
			if view != nil && view.GoalRef == ref && view.Phase == "paused" {
				return core.ToolResult{}, goalFailure("GOAL_TOOL_RESUME_PAUSED", "the user must resume a paused goal through /goal resume")
			}
		}
	case "complete", "blocked":
		if !authority.human && authority.round == nil {
			return core.ToolResult{}, goalFailure("GOAL_TOOL_AUTHORITY_REQUIRED", "complete and blocked require direct human input or the exact current goal round")
		}
		if hasObjective || cap != 0 || operation == "complete" && hasReason {
			return core.ToolResult{}, goalFailure("GOAL_TOOL_INVALID_UPDATE", "replacement fields belong only to edit; blocked_reason belongs only to blocked")
		}
		if operation == "blocked" {
			if !hasReason || strings.TrimSpace(*args.BlockedReason) == "" {
				return core.ToolResult{}, goalFailure("GOAL_TOOL_INVALID_UPDATE", "blocked_reason is required with action blocked")
			}
			_, threshold := a.goalDefaults()
			if !authority.human && authority.round.RoundsStarted < threshold {
				return core.ToolResult{}, goalFailure("GOAL_TOOL_BLOCK_THRESHOLD", fmt.Sprintf("blocked requires at least %d consecutive goal rounds; current round is %d", threshold, authority.round.RoundsStarted))
			}
			reason = &state.GoalBlockReason{Code: "model-reported", Message: *args.BlockedReason}
			operation = "block"
		}
	default:
		return core.ToolResult{}, goalFailure("GOAL_TOOL_INVALID_UPDATE", "unknown goal action")
	}
	if err = ctx.Err(); err != nil {
		return core.ToolResult{}, err
	}
	mutation, err := a.goalService().Update(tc.SessionID, tc.TurnID, ref, operation, edit, reason)
	if err != nil {
		return core.ToolResult{}, err
	}
	a.notice(agent.Notice{SessionID: tc.SessionID, Type: "event", Data: mutation.Event})
	if !authority.human && authority.round != nil && (operation == "complete" || operation == "block") {
		if tc.Inject == nil {
			return core.ToolResult{}, goalFailure("GOAL_TOOL_DRIVER_REQUIRED", "goal wrap-up context requires an active tool pipeline")
		}
		message := core.TextMessage("user", goalWrapup(mutation.View.Objective, reason))
		message.Source = map[string]any{"kind": "tool-goal", "form": "notice", "summary": args.Action + ": " + mutation.View.Objective}
		if err := tc.Inject(message); err != nil {
			return core.ToolResult{}, err
		}
	}
	return goalValue(mutation.View), nil
}
func goalWrapup(objective string, reason *state.GoalBlockReason) string {
	heading := "Objective: " + string(core.JSON(objective)) + "\n"
	grounding := "Report only what earlier rounds and tool results in this session actually establish; when a detail is not in the session, say so instead of inventing it. "
	if reason == nil {
		return "<goal_complete>\n" + heading + "The goal is marked complete and this autonomous run is ending. Write the closing message to the user now: state the outcome, summarize what was done and how it was verified, and point to the concrete results (files, commits, or other artifacts). " + grounding + "Note anything the user should review or do next. Address the user directly. Do not call any more tools in this run; further work waits for the user's next instruction.\n</goal_complete>"
	}
	return "<goal_blocked>\n" + heading + "Blocked: " + string(core.JSON(reason.Message)) + "\nThe goal is marked blocked and this autonomous run is ending. Write the closing message to the user now: state what has been completed so far, describe the concrete blocking condition and what you tried, and say exactly what you need from the user to continue. " + grounding + "Address the user directly. Do not call any more tools in this run; further work waits for the user's next instruction.\n</goal_blocked>"
}
