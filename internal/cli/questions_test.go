package cli

import (
	"bufio"
	"context"
	"reflect"
	"strings"
	"testing"

	"dsgo/internal/core"
)

func TestQuestionOtherPreservesNumericAndOptionLabelText(t *testing.T) {
	for _, custom := range []string{"2025", "1", "Approve", "", "1,2"} {
		t.Run(custom, func(t *testing.T) {
			a, s := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
				return core.Response{}, nil
			}))
			a.in = bufio.NewReader(strings.NewReader("3\n" + custom + "\n"))
			answers, err := a.askQuestions(a.ctx, core.ToolContext{SessionID: s.ID}, []userQuestion{{ID: "year", Question: "Choose a year", Options: []userQuestionOption{{Label: "2023"}, {Label: "2024"}}}})
			if err != nil {
				t.Fatal(err)
			}
			answer := answers.Answers[0]
			if len(answer.Selected) != 0 || answer.Custom == nil || *answer.Custom != custom {
				t.Fatalf("Other was interpreted as an option: %+v", answer)
			}
		})
	}
}

func TestQuestionMultiSelectCombinesOptionsAndOther(t *testing.T) {
	for _, selection := range []string{"1,3", "1,Other", "3,1,3"} {
		t.Run(selection, func(t *testing.T) {
			a, s := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
				return core.Response{}, nil
			}))
			a.in = bufio.NewReader(strings.NewReader(selection + "\n2025\n"))
			answers, err := a.askQuestions(a.ctx, core.ToolContext{SessionID: s.ID}, []userQuestion{{ID: "features", Question: "Choose features", MultiSelect: true, Options: []userQuestionOption{{Label: "A"}, {Label: "B"}}}})
			if err != nil {
				t.Fatal(err)
			}
			answer := answers.Answers[0]
			if !reflect.DeepEqual(answer.Selected, []string{"A"}) || answer.Custom == nil || *answer.Custom != "2025" {
				t.Fatalf("combined answer lost a field: %+v", answer)
			}
		})
	}
}

func TestQuestionSingleChoiceCannotCombineApprovalAndOther(t *testing.T) {
	q := userQuestion{ID: "plan", Options: []userQuestionOption{{Label: "Approve"}, {Label: "Keep planning"}}}
	for _, input := range []string{"1,3", "1,Other"} {
		if _, _, err := parseQuestionChoice(q, input); err == nil {
			t.Fatalf("single choice accepted %q", input)
		}
	}
}
