package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"dsgo/internal/agent"
	"dsgo/internal/core"
)

type planIntent struct{ active, narrate bool }
type planRuntime struct {
	mu      sync.Mutex
	pending map[string]planIntent
}
type planView struct {
	active, open bool
	lastHeader   *bool
}

// Plan mode is collaboration guidance. Permission and approval remain
// independent; selecting it does not cancel children or reset shells.
func (a *App) loggedPlan(id string) (planView, error) {
	s, err := a.store.Get(id)
	if err != nil {
		return planView{}, err
	}
	v := planView{active: s.Plan}
	events, err := a.store.Events(id)
	if err != nil {
		return v, err
	}
	for _, event := range events {
		switch event.Type {
		case "plan/mode", "plan/updated":
			var data struct {
				Active  *bool `json:"active"`
				Enabled *bool `json:"enabled"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return v, err
			}
			if data.Active != nil {
				v.active = *data.Active
			} else if data.Enabled != nil {
				v.active = *data.Enabled
			} else {
				return v, errors.New("invalid persisted plan mode")
			}
		case "plan/request":
			var data struct {
				Active bool `json:"active"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return v, err
			}
			v.lastHeader = &data.Active
		case "turn/started", "turn/start":
			v.open = true
		case "turn/finished", "turn/end":
			v.open = false
		}
	}
	return v, nil
}

func (a *App) planFor(id string) bool {
	if a.store == nil {
		return a.plan.Load()
	}
	v, err := a.loggedPlan(id)
	return err != nil || v.active
}

func (a *App) setPlan(id string, on bool) error {
	a.planState.mu.Lock()
	defer a.planState.mu.Unlock()
	v, err := a.loggedPlan(id)
	if err != nil {
		return err
	}
	if a.planState.pending == nil {
		a.planState.pending = map[string]planIntent{}
	}
	if v.open {
		if v.active == on {
			delete(a.planState.pending, id)
		} else {
			a.planState.pending[id] = planIntent{on, true}
		}
		return nil
	}
	if v.active == on {
		delete(a.planState.pending, id)
		return nil
	}
	if err := a.commitPlan(id, "", on); err != nil {
		return err
	}
	delete(a.planState.pending, id)
	if text := planNarration(v, on); text != "" && a.engine != nil {
		message := core.TextMessage("user", text)
		message.Source = map[string]any{"kind": "plan-mode", "form": "notice", "summary": text}
		_, err = a.engine.Inbox(context.Background(), id, message)
	}
	return err
}

func planNarration(v planView, target bool) string {
	if v.lastHeader == nil || *v.lastHeader == target {
		return ""
	}
	if target {
		return "The user switched this session to plan mode."
	}
	return "The user switched this session back to the default mode."
}

func (a *App) commitPlan(id, turn string, active bool) error {
	event, err := a.store.Append(id, turn, "plan/mode", map[string]any{"active": active})
	if err != nil {
		return err
	}
	// Session metadata is a compatibility mirror; the log is authoritative.
	if s, getErr := a.store.Get(id); getErr == nil {
		s.Plan = active
		if updateErr := a.store.Update(s); updateErr != nil {
			a.errorLine(fmt.Sprintf("plan metadata: %v", updateErr))
		}
	}
	if id == a.current.Load() {
		a.plan.Store(active)
	}
	a.notice(agent.Notice{SessionID: id, Type: "event", Data: event})
	return nil
}

// prepareStep runs before every model step: the instruction baseline first,
// then plan narration, and the time reading last, as the official
// time-context appends after other pre-step messages.
func (a *App) prepareStep(ctx context.Context, s core.Session, turn string, step int) error {
	if err := a.ensureInstructionBaseline(ctx, s, turn); err != nil {
		return err
	}
	if err := a.preparePlanStep(ctx, s, turn, step); err != nil {
		return err
	}
	return a.appendTimeContext(ctx, s, turn, step)
}

func (a *App) preparePlanStep(ctx context.Context, s core.Session, turn string, step int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.planState.mu.Lock()
	defer a.planState.mu.Unlock()
	intent, exists := a.planState.pending[s.ID]
	if !exists {
		return nil
	}
	v, err := a.loggedPlan(s.ID)
	if err != nil {
		return err
	}
	if v.active != intent.active {
		if err = a.commitPlan(s.ID, turn, intent.active); err != nil {
			// Keep a failed selection retryable without aborting admitted work.
			a.errorLine(fmt.Sprintf("plan mode append: %v", err))
			return nil
		}
	}
	delete(a.planState.pending, s.ID)
	if text := planNarration(v, intent.active); intent.narrate && text != "" {
		message := core.TextMessage("user", text)
		message.Source = map[string]any{"kind": "plan-mode", "form": "notice", "summary": text}
		event, err := a.store.Append(s.ID, turn, "user/message", message)
		if err != nil {
			return err
		}
		a.notice(agent.Notice{SessionID: s.ID, Type: "event", Data: event})
	}
	return nil
}

func (a *App) beforeModel(ctx context.Context, s core.Session, turn string, request core.Request) error {
	if err := a.beforeGoalModel(ctx, s, turn, request); err != nil {
		return err
	}
	v, err := a.loggedPlan(s.ID)
	if err != nil {
		return err
	}
	_, err = a.store.Append(s.ID, turn, "plan/request", map[string]any{"active": v.active})
	return err
}

func validPlanHeading(text string) bool {
	runes := []rune(strings.TrimFunc(text, jsWhitespace))
	if len(runes) < 3 || runes[0] != '#' || !jsWhitespace(runes[1]) {
		return false
	}
	for _, r := range runes[2:] {
		if !jsWhitespace(r) {
			return true
		}
	}
	return false
}

func (a *App) planGuidanceFor(id string) bool {
	a.planState.mu.Lock()
	intent, exists := a.planState.pending[id]
	a.planState.mu.Unlock()
	if exists {
		return intent.active
	}
	return a.planFor(id)
}

func (a *App) exitPlanMode(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	if !a.planFor(tc.SessionID) {
		return core.ToolResult{}, errors.New("exit_plan_mode is only available in plan mode")
	}
	var p struct {
		Plan string `json:"plan"`
	}
	if err := decodeOfficialObject(raw, &p); err != nil {
		return core.ToolResult{}, err
	}
	if !validPlanHeading(p.Plan) {
		return core.ToolResult{}, errors.New("exit_plan_mode requires a non-empty markdown plan starting with a # heading")
	}
	// Only the explicit Approve choice leaves plan mode; any typed text is
	// feedback for the next revision.
	review := userQuestion{ID: "plan-review", Question: p.Plan + "\n\nApprove this plan and leave plan mode?", Options: []userQuestionOption{
		{Label: "Approve", Description: "Leave plan mode and carry out the plan from the next step."},
		{Label: "Keep planning", Description: "Send feedback to revise the plan."},
	}}
	answers, err := a.askQuestions(ctx, tc, []userQuestion{review})
	if err != nil {
		return core.ToolResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return core.ToolResult{}, err
	}
	answer := answers.Answers[0]
	if answer.Custom != nil || len(answer.Selected) != 1 || answer.Selected[0] != "Approve" {
		feedback := "none"
		if answer.Custom != nil {
			feedback = strings.TrimSpace(*answer.Custom)
		}
		return core.ToolResult{}, fmt.Errorf("the user chose to keep planning; their feedback: %s", feedback)
	}
	a.planState.mu.Lock()
	if a.planState.pending == nil {
		a.planState.pending = map[string]planIntent{}
	}
	a.planState.pending[tc.SessionID] = planIntent{active: false, narrate: false}
	a.planState.mu.Unlock()
	return toolValue(map[string]any{"approved": true}), nil
}

func inheritPlan(child *core.Session, parent core.Session) { child.Plan = parent.Plan }
