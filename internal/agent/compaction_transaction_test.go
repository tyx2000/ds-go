package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"dsgo/internal/core"
)

func TestCompactionCASPreservesSystemTailOriginsAndStructuredReplay(t *testing.T) {
	store, session := setup(t)
	image := core.Block{Type: "image", Source: map[string]any{"type": "base64", "media_type": "image/png", "data": "fixture-image"}}
	input := core.Message{Role: "user", Content: []core.Block{{Type: "text", Text: strings.Repeat("important diagram requirement ", 100)}, image}}
	snapshot := core.TextMessage("user", "captured immutable source")
	snapshot.Source = map[string]any{"kind": "session-reference"}
	for _, event := range []struct {
		kind string
		data any
	}{
		{"system/message", core.TextMessage("system", "durable system head")},
		{"user/message", input},
		{"assistant/message", core.TextMessage("assistant", strings.Repeat("diagram description ", 80))},
	} {
		if _, err := store.Append(session.ID, "old", event.kind, event.data); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.AppendInputBatch(session.ID, "current", core.TextMessage("user", "keep recent direct"), []core.Message{snapshot}); err != nil {
		t.Fatal(err)
	}
	before, err := store.ContextSurface(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	tools := []core.ToolSpec{{Name: "read", Description: "routed schema", InputSchema: core.JSON(map[string]any{"type": "object"})}}
	target := core.Request{Provider: "routed-provider", Model: "vision-model", System: "effective runtime system", Tools: tools, MaxTokens: 1234}
	model := fakeModel{func(_ context.Context, req core.Request, _ func(core.Delta)) (core.Response, error) {
		if req.Provider != target.Provider || req.Model != target.Model || req.System != target.System || !reflect.DeepEqual(req.Tools, tools) {
			t.Fatalf("summary did not preserve actual routed envelope: %#v", req)
		}
		if len(req.Messages) != 4 || req.Messages[0].Role != "system" || req.Messages[1].Content[1].Type != "image" || string(core.JSON(req.Messages[1].Content[1].Source)) != string(core.JSON(image.Source)) {
			t.Fatalf("summary did not replay real system/image prefix: %#v", req.Messages)
		}
		if req.Messages[len(req.Messages)-1].Role != "user" || req.Messages[len(req.Messages)-1].Content[0].Text != compactionInstruction {
			t.Fatal("directive is not the final user message")
		}
		if strings.Contains(string(core.JSON(req.Messages)), "keep recent direct") {
			t.Fatal("summary included retained tail")
		}
		return core.Response{Content: []core.Block{{Type: "text", Text: "diagram checkpoint"}}, StopReason: "end_turn"}, nil
	}}
	engine := New(store, model, &fakeTools{}, Options{})
	defer engine.Close()
	if err = engine.compactAt(context.Background(), session.ID, "compact", core.ContextMessages(before), 3, target); err != nil {
		t.Fatal(err)
	}
	after, err := store.ContextSurface(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 4 || after[0].Origin != before[0].Origin || !reflect.DeepEqual(after[2:], before[3:]) {
		t.Fatalf("CAS rewrote retained origins/groups: before=%#v after=%#v", before, after)
	}
	if after[1].Message.Role != "user" || after[1].InputGroup != 0 || !strings.Contains(string(core.JSON(after[1].Message.Source)), "compact-checkpoint") {
		t.Fatal("checkpoint gained direct-input authority")
	}
	if after[2].InputGroup == 0 || after[2].InputGroup != after[3].InputGroup {
		t.Fatal("retained direct input lost its captured context")
	}
}

func TestCompactionCASDoesNotOverwriteMessagesAppendedDuringSummary(t *testing.T) {
	store, session := setup(t)
	original := core.TextMessage("user", strings.Repeat("original requirement ", 100))
	_, _ = store.Append(session.ID, "old", "user/message", original)
	model := fakeModel{func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		if _, err := store.Append(session.ID, "concurrent", "user/message", core.TextMessage("user", "arrived during summary")); err != nil {
			t.Fatal(err)
		}
		return core.Response{Content: []core.Block{{Type: "text", Text: "short summary"}}}, nil
	}}
	engine := New(store, model, &fakeTools{}, Options{})
	defer engine.Close()
	if err := engine.compactAt(context.Background(), session.ID, "compact", []core.Message{original}, 1); err == nil {
		t.Fatal("stale whole-surface summary was committed")
	}
	messages, err := store.Messages(session.ID)
	if err != nil || len(messages) != 2 || messages[1].Content[0].Text != "arrived during summary" {
		t.Fatal("concurrent input was shadowed", messages, err)
	}
	events, _ := store.Events(session.ID)
	if contextMutationRevision(events) != 0 {
		t.Fatal("failed CAS published a replacement")
	}
}

func TestCompactionCancellationRejectsOtherwiseValidSummary(t *testing.T) {
	store, session := setup(t)
	longHistory(t, store, session.ID, 6, 12000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ordinary := 0
	model := fakeModel{func(_ context.Context, req core.Request, _ func(core.Delta)) (core.Response, error) {
		if req.Purpose == "compaction" {
			cancel()
			return core.Response{Content: []core.Block{{Type: "text", Text: "valid summary"}}}, nil
		}
		ordinary++
		return core.Response{Content: []core.Block{{Type: "text", Text: "done"}}}, nil
	}}
	engine := New(store, model, &fakeTools{}, Options{Model: "test-model", ContextTokens: 100000, MaxTokens: 100})
	defer engine.Close()
	if err := engine.Run(ctx, session.ID, core.TextMessage("user", "next")); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost precedence", err)
	}
	events, _ := store.Events(session.ID)
	if ordinary != 0 || contextMutationRevision(events) != 0 {
		t.Fatal("cancelled summary was committed or sent to ordinary model")
	}
}

func TestAfterCompactionFailureIsRequiredPreparationFailure(t *testing.T) {
	store, session := setup(t)
	longHistory(t, store, session.ID, 6, 12000)
	failure := errors.New("instruction baseline persistence failed")
	ordinary, callback := 0, 0
	model := fakeModel{func(_ context.Context, req core.Request, _ func(core.Delta)) (core.Response, error) {
		if req.Purpose == "compaction" {
			return core.Response{Content: []core.Block{{Type: "text", Text: "summary"}}}, nil
		}
		ordinary++
		return core.Response{Content: []core.Block{{Type: "text", Text: "done"}}}, nil
	}}
	engine := New(store, model, &fakeTools{}, Options{Model: "test-model", ContextTokens: 100000, MaxTokens: 100, AfterCompaction: func(context.Context, core.Session, string) error { callback++; return failure }})
	defer engine.Close()
	if err := engine.Run(context.Background(), session.ID, core.TextMessage("user", "next")); !errors.Is(err, failure) {
		t.Fatal("mandatory producer failure was swallowed", err)
	}
	events, _ := store.Events(session.ID)
	if ordinary != 0 || callback != 1 || contextMutationRevision(events) != 1 {
		t.Fatalf("ordinary=%d callback=%d mutations=%d", ordinary, callback, contextMutationRevision(events))
	}
}

func TestOverflowRetriesSameStepAfterPruneProgressAndSummaryFailure(t *testing.T) {
	store, session := setup(t)
	turn := ID()
	for _, event := range []struct {
		kind string
		data any
	}{
		{"turn/started", map[string]any{"id": turn}},
		{"user/message", core.TextMessage("user", "read output")},
		{"assistant/message", core.Message{Role: "assistant", Content: []core.Block{{Type: "tool_use", ID: "call", Name: "test", Input: core.JSON(map[string]any{})}}}},
		{"tool/result", core.Message{Role: "user", Content: []core.Block{{Type: "tool_result", ToolUseID: "call", Content: strings.Repeat("large tool result\n", 2000)}}}},
		{"assistant/message", core.TextMessage("assistant", "read output complete")},
		{"turn/finished", map[string]any{"status": "completed"}},
	} {
		if _, err := store.Append(session.ID, turn, event.kind, event.data); err != nil {
			t.Fatal(err)
		}
	}
	ordinary, summaries, prepares, starts, warnings := 0, 0, 0, 0, 0
	model := fakeModel{func(_ context.Context, req core.Request, _ func(core.Delta)) (core.Response, error) {
		if req.Purpose == "compaction" {
			summaries++
			return core.Response{}, errors.New("summary service unavailable")
		}
		ordinary++
		if ordinary == 1 {
			return core.Response{}, errors.New("context_window_exceeded")
		}
		if !strings.Contains(string(core.JSON(req.Messages)), "tool result middle pruned") {
			t.Fatal("retry used the stale unpruned request")
		}
		return core.Response{Content: []core.Block{{Type: "text", Text: "done"}}}, nil
	}}
	engine := New(store, model, &fakeTools{}, Options{Model: "test-model", ContextTokens: 1000000, MaxTokens: 100, MaxSteps: 1, BeforeStep: func(context.Context, core.Session, string, int) error { prepares++; return nil }})
	defer engine.Close()
	engine.Notify = func(notice Notice) {
		if notice.Type == "warning" {
			warnings++
		}
		if event, ok := notice.Data.(core.Event); ok && event.Type == "step/started" {
			starts++
		}
	}
	if err := engine.Run(context.Background(), session.ID, core.TextMessage("user", "continue")); err != nil {
		t.Fatal(err)
	}
	if ordinary != 2 || summaries != 1 || prepares != 1 || starts != 1 || warnings != 1 {
		t.Fatalf("ordinary=%d summaries=%d prepares=%d starts=%d warnings=%d", ordinary, summaries, prepares, starts, warnings)
	}
}
