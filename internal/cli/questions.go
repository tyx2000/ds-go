package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"dsgo/internal/core"
)

type userQuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}
type userQuestion struct {
	ID          string               `json:"id"`
	Question    string               `json:"question"`
	Header      string               `json:"header,omitempty"`
	Detail      string               `json:"detail,omitempty"`
	Options     []userQuestionOption `json:"options,omitempty"`
	MultiSelect bool                 `json:"multi_select,omitempty"`
}
type userAnswer struct {
	ID       string   `json:"id"`
	Selected []string `json:"selected"`
	Custom   *string  `json:"custom,omitempty"`
}
type userAnswers struct {
	Answers []userAnswer `json:"answers"`
}

func userQuestionSpec() core.ToolSpec {
	return toolSpec("ask_user_question", "Ask the user a concise question when you need confirmation, a choice, or missing information before proceeding.", `{"type":"object","required":["questions"],"properties":{"questions":{"type":"array","items":{"type":"object","additionalProperties":true,"required":["id","question"],"properties":{"id":{"type":"string"},"question":{"type":"string"},"header":{"type":"string"},"options":{"type":"array","items":{"type":"object","additionalProperties":true,"required":["label"],"properties":{"label":{"type":"string"},"description":{"type":"string"}}}},"multi_select":{"type":"boolean"}}}}}}`)
}

func (a *App) question(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	var p struct {
		Questions []userQuestion `json:"questions"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return core.ToolResult{}, err
	}
	answers, err := a.askQuestions(ctx, tc, p.Questions)
	if err != nil {
		return core.ToolResult{}, err
	}
	return toolValue(answers), nil
}

func (a *App) askQuestions(ctx context.Context, tc core.ToolContext, questions []userQuestion) (userAnswers, error) {
	result := userAnswers{Answers: []userAnswer{}}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if len(questions) == 0 {
		return result, errors.New("ask_user_question requires at least one question")
	}
	if tc.SessionID != "" && !a.goalRuntimeRoot(tc.SessionID) {
		return result, errors.New("delegated agents cannot ask user questions; report the choice to the delegating agent")
	}
	if a.unattended || a.flags.json {
		return result, errors.New("user question requires interactive input")
	}
	a.inputMu.Lock()
	defer a.inputMu.Unlock()
	for _, q := range questions {
		if q.Header != "" {
			a.line("\n%s\n", clean(q.Header))
		}
		if q.Detail != "" {
			a.line("\n%s\n", clean(q.Detail))
		}
		a.line("\n%s\n", clean(q.Question))
		for i, option := range q.Options {
			a.line("  %d. %s\n", i+1, clean(option.Label))
			if option.Description != "" {
				a.line("     %s\n", clean(option.Description))
			}
		}
		prompt := "Answer: "
		if len(q.Options) > 0 {
			prompt = "Choose a number or type your answer: "
			if q.MultiSelect {
				prompt = "Choose numbers separated by commas, or type your answer: "
			}
		}
		for {
			a.line("%s", prompt)
			text, err := a.readPrivateLine(ctx, prompt)
			if err != nil {
				return result, err
			}
			if err := ctx.Err(); err != nil {
				return result, err
			}
			answer, err := parseQuestionAnswer(q, text)
			if err != nil {
				a.line("%s\n", err)
				continue
			}
			result.Answers = append(result.Answers, answer)
			break
		}
	}
	if tc.Emit != nil {
		if err := tc.Emit("user/answer", result); err != nil {
			return userAnswers{}, err
		}
	}
	return result, nil
}

func parseQuestionAnswer(q userQuestion, text string) (userAnswer, error) {
	answer := userAnswer{ID: q.ID, Selected: []string{}}
	trimmed := strings.TrimFunc(text, jsWhitespace)
	if len(q.Options) == 0 {
		answer.Custom = &text
		return answer, nil
	}
	if trimmed == "" {
		return answer, errors.New("select an option or enter an answer")
	}
	for _, option := range q.Options {
		if trimmed == option.Label {
			answer.Selected = append(answer.Selected, option.Label)
			return answer, nil
		}
	}
	parts := strings.Split(trimmed, ",")
	indexes := []int{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		index, err := strconv.Atoi(part)
		if err != nil {
			answer.Custom = &text
			return answer, nil
		}
		if index < 1 || index > len(q.Options) {
			return answer, fmt.Errorf("choose a number from 1 to %d", len(q.Options))
		}
		indexes = append(indexes, index-1)
	}
	if !q.MultiSelect && len(indexes) > 1 {
		return answer, errors.New("this question accepts one option")
	}
	seen := map[int]bool{}
	for _, index := range indexes {
		if !seen[index] {
			answer.Selected = append(answer.Selected, q.Options[index].Label)
			seen[index] = true
		}
	}
	return answer, nil
}
