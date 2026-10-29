package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"dsgo/internal/core"
	"dsgo/internal/tools"
)

func captureJobBound(a *App, tc core.ToolContext, name string, limit int) {
	a.jobOutputBounds.Store(jobBoundKey(tc, name), jobOutputBound{"removed-job", limit})
}

func TestJobOutputFinalizerKeepsCanonicalForPTCAndCapturedRemoval(t *testing.T) {
	a := &App{registry: tools.New()}
	defer a.registry.Close()
	tc := core.ToolContext{SessionID: "owner", TurnID: "turn", ExecutionID: "exec", CallID: "read", Nested: true}
	canonical := core.JSON(map[string]any{"text": strings.Repeat("文", 100) + "tail", "job": map[string]string{"id": "removed-job", "status": "completed", "detail": "exit code: 0"}})
	captureJobBound(a, tc, "job_output", 90)
	res, err := a.finishJobOutputBound(tc, "job_output", core.ToolResult{Value: canonical, Content: string(canonical)}, nil)
	if err != nil || len(res.Content) > 90 || !utf8.ValidString(res.Content) || !strings.HasSuffix(res.Content, "[status: completed, exit code: 0]") {
		t.Fatalf("output not bounded with removed producer: %+v %v", res, err)
	}
	if string(res.Value) != string(canonical) || !json.Valid(res.Value) || len(res.Value) <= 90 {
		t.Fatalf("PTC canonical value was truncated: %s", res.Value)
	}
	unchanged := core.ToolResult{Content: strings.Repeat("x", 200)}
	again, _ := a.finishJobOutputBound(tc, "job_output", unchanged, nil)
	if again.Content != unchanged.Content {
		t.Fatal("capture reused after finalization")
	}
}

func TestJobFinalizerPolicyErrorsPreserveCauseAndBlocks(t *testing.T) {
	a := &App{}
	tc := core.ToolContext{SessionID: "owner", ExecutionID: "exec", CallID: "call"}
	captureJobBound(a, tc, "job_kill", 64)
	cause := context.Canceled
	err := &jobFixtureError{cause, strings.Repeat("policy denied ", 30)}
	res, gotErr := a.finishJobOutputBound(tc, "job_kill", core.ToolResult{}, err)
	var typed *jobFixtureError
	if !errors.Is(gotErr, context.Canceled) || !errors.As(gotErr, &typed) || !res.IsError || len(gotErr.Error()) > 64 || gotErr.Error() != res.Content || !strings.HasSuffix(res.Content, "[result truncated]") {
		t.Fatalf("error cause or cap lost: %+v %v", res, gotErr)
	}
	blocks := []core.Block{{Type: "text", Text: strings.Repeat("x", 100)}, {Type: "text", Text: "policy"}}
	captureJobBound(a, tc, "job_output", 64)
	res, _ = a.finishJobOutputBound(tc, "job_output", core.ToolResult{Content: "unchanged", Blocks: blocks}, nil)
	if res.Content != "unchanged" || res.Blocks[0].Text != blocks[0].Text {
		t.Fatal("structured policy content was flattened")
	}
	block := []core.Block{{Type: "text", Text: strings.Repeat("policy ", 40)}}
	captureJobBound(a, tc, "job_output", 64)
	res, _ = a.finishJobOutputBound(tc, "job_output", core.ToolResult{Blocks: block}, nil)
	if len(res.Blocks[0].Text) > 64 || block[0].Text == res.Blocks[0].Text || !strings.HasSuffix(res.Blocks[0].Text, "[result truncated]") {
		t.Fatal("single-text block was not independently bounded")
	}
}

type jobFixtureError struct {
	cause error
	text  string
}

func (e *jobFixtureError) Error() string { return e.text }
func (e *jobFixtureError) Unwrap() error { return e.cause }

func TestJobBoundOwnershipKeyAndUncappedResult(t *testing.T) {
	a := &App{registry: tools.New()}
	defer a.registry.Close()
	tc := core.ToolContext{SessionID: "owner", TurnID: "turn", ExecutionID: "exec", CallID: "call"}
	captureJobBound(a, tc, "job_output", 64)
	other := tc
	other.ExecutionID = "other-exec"
	input := core.ToolResult{Content: strings.Repeat("output ", 30)}
	res, _ := a.finishJobOutputBound(other, "job_output", input, nil)
	if res.Content != input.Content {
		t.Fatal("separate execution inherited cap")
	}
	a.beginJobOutputBound(other, "job_output", core.JSON(map[string]string{"job_id": "absent"}))
	res, _ = a.finishJobOutputBound(other, "job_output", input, nil)
	if res.Content != input.Content {
		t.Fatal("missing producer acquired a default cap")
	}
}
