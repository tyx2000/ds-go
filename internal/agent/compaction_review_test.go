package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dsgo/internal/core"
)

func TestReviewPressureSummaryFailureDoesNotAbortUsableRequest(t *testing.T) {
	store, s := setup(t)
	longHistory(t, store, s.ID, 6, 12000)
	agentCalls := 0
	model := fakeModel{func(_ context.Context, req core.Request, _ func(core.Delta)) (core.Response, error) {
		if req.Purpose == "compaction" {
			return core.Response{}, errors.New("summary endpoint temporarily unavailable")
		}
		agentCalls++
		return core.Response{Content: []core.Block{{Type: "text", Text: "usable request succeeded"}}}, nil
	}}
	e := New(store, model, &fakeTools{}, Options{Model: "test-model", ContextTokens: 100000, MaxTokens: 100})
	defer e.Close()
	if err := e.Run(context.Background(), s.ID, core.TextMessage("user", "continue")); err != nil {
		t.Fatalf("proactive summary failure aborted normal turn before model call (calls=%d): %v", agentCalls, err)
	}
	if agentCalls != 1 {
		t.Fatalf("agent model calls=%d", agentCalls)
	}
}

func TestReviewCompactionRejectsInvalidCheckpoint(t *testing.T) {
	for _, test := range []struct{ name, text, stop string }{{"truncated", "INCOMPLETE", "max_tokens"}, {"blank", "  \n ", "end_turn"}, {"larger", strings.Repeat("EXPANDED", 1000), "end_turn"}} {
		t.Run(test.name, func(t *testing.T) {
			store, s := setup(t)
			original := core.TextMessage("user", "IMPORTANT_ORIGINAL_RULE")
			_, _ = store.Append(s.ID, "old", "user/message", original)
			model := fakeModel{func(_ context.Context, _ core.Request, _ func(core.Delta)) (core.Response, error) {
				return core.Response{Content: []core.Block{{Type: "text", Text: test.text}}, StopReason: test.stop}, nil
			}}
			e := New(store, model, &fakeTools{}, Options{Model: "test-model"})
			defer e.Close()
			if err := e.compactAt(context.Background(), s.ID, "now", []core.Message{original}, 1); err == nil {
				t.Fatal("invalid checkpoint was durably accepted and shadowed the original context")
			}
		})
	}
}

func TestReviewPrunerAfterCompactionKeepsTailGroupsIndependent(t *testing.T) {
	store, s := setup(t)
	messages := []core.Message{
		core.TextMessage("user", strings.Repeat("old ", 300)), core.TextMessage("assistant", strings.Repeat("old answer ", 100)), core.TextMessage("user", "run tool"),
		{Role: "assistant", Content: []core.Block{{Type: "tool_use", ID: "call1", Name: "test", Input: core.JSON(map[string]any{})}}},
		{Role: "user", Content: []core.Block{{Type: "tool_result", ToolUseID: "call1", Content: strings.Repeat("log data\n", 2000)}}},
		core.TextMessage("assistant", "tool done"), core.TextMessage("user", "next task"),
	}
	for _, message := range messages {
		kind := "user/message"
		if message.Role == "assistant" {
			kind = "assistant/message"
		}
		if len(message.Content) > 0 && message.Content[0].Type == "tool_result" {
			kind = "tool/result"
		}
		if _, err := store.Append(s.ID, "old", kind, message); err != nil {
			t.Fatal(err)
		}
	}
	model := fakeModel{func(_ context.Context, _ core.Request, _ func(core.Delta)) (core.Response, error) {
		return core.Response{Content: []core.Block{{Type: "text", Text: "summary"}}}, nil
	}}
	e := New(store, model, &fakeTools{}, Options{Model: "test-model"})
	defer e.Close()
	if err := e.compactAt(context.Background(), s.ID, "now", messages, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pruneToolResults(context.Background(), s.ID, "later"); err != nil {
		t.Fatalf("pruner cannot replace an independent retained tool result: %v", err)
	}
}

func TestReviewOverflowRetryDoesNotConsumeStepBudget(t *testing.T) {
	store, s := setup(t)
	longHistory(t, store, s.ID, 3, 200)
	requests := &recordedRequests{}
	e := New(store, requests.model(func(n int) error {
		if n == 1 {
			return errors.New("context_window_exceeded")
		}
		return nil
	}), &fakeTools{}, Options{Model: "test-model", MaxTokens: 100, MaxSteps: 1})
	defer e.Close()
	if err := e.Run(context.Background(), s.ID, core.TextMessage("user", "next")); err != nil {
		t.Fatalf("same-step overflow retry failed, requests=%d: %v", len(requests.agent), err)
	}
	if len(requests.agent) != 2 {
		t.Fatalf("model requests=%d", len(requests.agent))
	}
}

func TestReviewCompactionRetainsActualImageBlocks(t *testing.T) {
	store, s := setup(t)
	prefix := core.Message{Role: "user", Content: []core.Block{{Type: "text", Text: strings.Repeat("Remember the diagram requirements. ", 50)}, {Type: "image", Source: map[string]any{"type": "base64", "media_type": "image/png", "data": "fixture"}}}}
	_, _ = store.Append(s.ID, "old", "user/message", prefix)
	_, _ = store.Append(s.ID, "old", "assistant/message", core.TextMessage("assistant", "I inspected it"))
	model := fakeModel{func(_ context.Context, req core.Request, _ func(core.Delta)) (core.Response, error) {
		found := false
		for _, message := range req.Messages {
			for _, block := range message.Content {
				found = found || block.Type == "image"
			}
		}
		if !found {
			t.Error("summary request contains only a JSON transcript, no model-readable image block")
		}
		return core.Response{Content: []core.Block{{Type: "text", Text: "summary"}}}, nil
	}}
	e := New(store, model, &fakeTools{}, Options{Model: "test-model"})
	defer e.Close()
	if err := e.compactAt(context.Background(), s.ID, "now", []core.Message{prefix, core.TextMessage("assistant", "I inspected it")}, 2); err != nil {
		t.Fatal(err)
	}
}
