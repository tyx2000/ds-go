package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"dsgo/internal/core"
)

type goal struct {
	Objective   string `json:"objective"`
	Status      string `json:"status"`
	TokenBudget int    `json:"token_budget,omitempty"`
	StartedSeq  int64  `json:"started_seq"`
}

func (a *App) goalState(id string) (goal, int, error) {
	var g goal
	events, err := a.store.Events(id)
	if err != nil {
		return g, 0, err
	}
	for _, ev := range events {
		if ev.Type == "goal/updated" {
			var next goal
			if json.Unmarshal(ev.Data, &next) != nil {
				continue
			}
			if next.Objective != "" {
				g = next
				if g.StartedSeq == 0 {
					g.StartedSeq = ev.Seq
				}
			}
		}
	}
	tokens := 0
	for _, ev := range events {
		if ev.Seq > g.StartedSeq && ev.Type == "step/usage" {
			var usage core.Usage
			if json.Unmarshal(ev.Data, &usage) == nil {
				tokens += usage.InputTokens + usage.OutputTokens
			}
		}
	}
	return g, tokens, nil
}

func (a *App) goalCommand(arg string) error {
	g, tokens, err := a.goalState(a.session.ID)
	if err != nil {
		return err
	}
	if arg == "" || arg == "status" {
		a.print(map[string]any{"goal": g, "used_tokens": tokens})
		return nil
	}
	if arg == "pause" || arg == "resume" || arg == "clear" {
		if g.Objective == "" {
			return errors.New("no goal exists")
		}
		switch arg {
		case "pause":
			g.Status = "paused"
		case "resume":
			g.Status = "active"
		case "clear":
			g.Status = "complete"
		}
		if _, err = a.store.Append(a.session.ID, "", "goal/updated", g); err != nil {
			return err
		}
		if arg == "resume" {
			return a.turn("Resume the user-requested goal: " + g.Objective)
		}
		return nil
	}
	if strings.HasPrefix(arg, "{") {
		if err = json.Unmarshal([]byte(arg), &g); err != nil {
			return err
		}
	} else {
		g = goal{Objective: arg}
	}
	if strings.TrimSpace(g.Objective) == "" || g.TokenBudget < 0 {
		return errors.New("goal requires an objective and nonnegative token_budget")
	}
	g.Status = "active"
	g.StartedSeq = 0
	event, err := a.store.Append(a.session.ID, "", "goal/updated", g)
	if err != nil {
		return err
	}
	g.StartedSeq = event.Seq
	return a.turn("The user explicitly requests sustained work on this goal: " + g.Objective + "\nUse update_goal to report complete only when achieved, or blocked when progress requires user input. Continue working while active.")
}

func (a *App) updateGoal(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	g, _, err := a.goalState(tc.SessionID)
	if err != nil {
		return core.ToolResult{}, err
	}
	if g.Objective == "" {
		return core.ToolResult{}, errors.New("no user-created goal exists; the model cannot create one")
	}
	var p struct{ Objective, Status string }
	if err = json.Unmarshal(raw, &p); err != nil {
		return core.ToolResult{}, err
	}
	if strings.TrimSpace(p.Objective) != strings.TrimSpace(g.Objective) {
		return core.ToolResult{}, errors.New("the model cannot replace the user's objective")
	}
	switch p.Status {
	case "active", "complete", "paused", "blocked":
	default:
		return core.ToolResult{}, errors.New("invalid goal status")
	}
	g.Status = p.Status
	if tc.Emit == nil {
		return core.ToolResult{}, errors.New("goal updates require durable event recording")
	}
	if err = tc.Emit("goal/updated", g); err != nil {
		return core.ToolResult{}, err
	}
	return toolValue(g), nil
}

func (a *App) goalContinuation() (string, error) {
	g, tokens, err := a.goalState(a.session.ID)
	if err != nil {
		return "", err
	}
	if g.Status != "active" || g.Objective == "" {
		return "", nil
	}
	if g.TokenBudget > 0 && tokens >= g.TokenBudget {
		g.Status = "paused"
		if _, err = a.store.Append(a.session.ID, "", "goal/updated", g); err != nil {
			return "", err
		}
		a.line("Goal paused after reaching its configured token budget (%d/%d).\n", tokens, g.TokenBudget)
		return "", nil
	}
	return fmt.Sprintf("Continue the active user-requested goal: %s\nInspect the current state and keep working. Use update_goal with the unchanged objective and status complete only when achieved, or blocked if user input is required. Do not claim completion without evidence.", g.Objective), nil
}
