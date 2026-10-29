package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"dsgo/internal/core"
	"dsgo/internal/state"
	"dsgo/internal/tools"
)

var structuredCountSchema = json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"],"additionalProperties":false}`)

func structuredCall(id, raw string) core.Block {
	return core.Block{Type: "tool_use", ID: id, Name: StructuredOutputTool, Input: json.RawMessage(raw)}
}

func TestStructuredOutputValidatesRetriesConcludesAndGuardsLaterCalls(t *testing.T) {
	store, session := setup(t)
	dispatch := &fakeTools{}
	requests := 0
	model := fakeModel{func(_ context.Context, request core.Request, _ func(core.Delta)) (core.Response, error) {
		requests++
		var schema json.RawMessage
		for _, spec := range request.Tools {
			if spec.Name == StructuredOutputTool {
				schema = spec.InputSchema
			}
		}
		if string(schema) != string(structuredCountSchema) || strings.Count(request.System, structuredOutputInstruction) != 1 {
			t.Fatalf("missing scoped schema or duplicate instruction: %s %s", schema, request.System)
		}
		if requests == 1 {
			return core.Response{Content: []core.Block{structuredCall("same-call", `{"count":"invalid"}`)}}, nil
		}
		if requests != 2 || !request.Messages[len(request.Messages)-1].Content[0].IsError {
			t.Fatal("invalid args did not retry within the same turn")
		}
		return core.Response{Content: []core.Block{structuredCall("same-call", `{"count":3}`), {Type: "tool_use", ID: "late", Name: "test", Input: json.RawMessage(`{}`)}}}, nil
	}}
	engine := New(store, model, dispatch, Options{System: "base", MaxSteps: 4})
	handle, release, err := engine.AttachStructured(session.ID, structuredCountSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err = engine.Run(context.Background(), session.ID, core.TextMessage("user", "count")); err != nil {
		t.Fatal(err)
	}
	value, captured := handle.Captured()
	if !captured || string(value) != `{"count":3}` || dispatch.calls != 0 || requests != 2 {
		t.Fatalf("capture=%s %v dispatch=%d requests=%d", value, captured, dispatch.calls, requests)
	}
	if _, err := engine.Inbox(context.Background(), session.ID, core.TextMessage("user", "too late")); err == nil {
		t.Fatal("committed structured output accepted another input")
	}
	release()
	if _, ok := engine.StructuredSpec(session.ID); ok || engine.StructuredInstruction(session.ID) != "" {
		t.Fatal("activation schema survived release")
	}
	if _, err := engine.Inbox(context.Background(), session.ID, core.TextMessage("user", "follow-up")); err != nil {
		t.Fatal("later continuation retained structured restriction", err)
	}
}

func TestStructuredOutputPlainTextDoesNotCountAsCapture(t *testing.T) {
	store, session := setup(t)
	model := fakeModel{func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		return core.Response{Content: []core.Block{{Type: "text", Text: `{"count":3}`}}}, nil
	}}
	engine := New(store, model, &fakeTools{}, Options{})
	handle, release, err := engine.AttachStructured(session.ID, structuredCountSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := engine.Run(context.Background(), session.ID, core.TextMessage("user", "count")); err != nil {
		t.Fatal(err)
	}
	if _, ok := handle.Captured(); ok {
		t.Fatal("plain text impersonated structured capture")
	}
}

type structuredAuditFailure struct{ Store }

func (s structuredAuditFailure) Append(id, turn, kind string, value any) (core.Event, error) {
	if kind == "tool/finished" {
		return core.Event{}, errors.New("authoritative audit failed")
	}
	return s.Store.Append(id, turn, kind, value)
}

func TestStructuredOutputCommitsOnlyAfterAuthoritativeResult(t *testing.T) {
	for _, failure := range []string{"rendered-error", "audit-error"} {
		t.Run(failure, func(t *testing.T) {
			store, session := setup(t)
			model := fakeModel{func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
				return core.Response{Content: []core.Block{structuredCall("output", `{"count":3}`)}}, nil
			}}
			var actualStore Store = store
			opts := Options{MaxSteps: 1}
			if failure == "audit-error" {
				actualStore = structuredAuditFailure{store}
			} else {
				opts.RenderToolResult = func(_ context.Context, _ core.ToolContext, _ string, result core.ToolResult) (core.ToolResult, error) {
					result.IsError = true
					return result, nil
				}
			}
			engine := New(actualStore, model, &fakeTools{}, opts)
			handle, release, err := engine.AttachStructured(session.ID, structuredCountSchema, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if err := engine.Run(context.Background(), session.ID, core.TextMessage("user", "count")); err == nil {
				t.Fatal("failed result settled as success")
			}
			if _, ok := handle.Captured(); ok || !handle.AcceptsInput() {
				t.Fatal("failed result committed or left input sealed")
			}
		})
	}
}

func TestStructuredOutputSealsInputAndWaitsForSiblingDispatch(t *testing.T) {
	store, session := setup(t)
	engine := New(store, fakeModel{}, &fakeTools{}, Options{})
	handle, release, err := engine.AttachStructured(session.ID, structuredCountSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx := context.Background()
	outer := core.ToolContext{SessionID: session.ID, TurnID: "turn", CallID: "code", ExecutionID: "execution"}
	if _, handled, err := engine.ExecuteStructured(ctx, outer, "run_code", nil); handled || err != nil {
		t.Fatal(handled, err)
	}
	sibling := outer
	sibling.Nested, sibling.CallID = true, "code/1"
	if _, handled, err := engine.ExecuteStructured(ctx, sibling, "subagent", nil); handled || err != nil {
		t.Fatal(handled, err)
	}
	output := outer
	output.Nested, output.CallID = true, "code/2"
	if _, _, err := engine.ExecuteStructured(ctx, output, StructuredOutputTool, json.RawMessage(`{"count":3}`)); err == nil {
		t.Fatal("capture overtook an admitted sibling dispatch")
	}
	engine.structuredSubresult(session.ID, "execution", map[string]any{"call_id": sibling.CallID, "result": core.ToolResult{Content: "done"}})
	if _, handled, err := engine.ExecuteStructured(ctx, output, StructuredOutputTool, json.RawMessage(`{"count":3}`)); !handled || err != nil {
		t.Fatal(handled, err)
	}
	if handle.AcceptsInput() {
		t.Fatal("staged capture left input open")
	}
	if _, err := engine.Inbox(ctx, session.ID, core.TextMessage("user", "late")); err == nil {
		t.Fatal("staged capture accepted input")
	}
	if _, handled, err := engine.ExecuteStructured(ctx, sibling, "write", nil); !handled || err == nil {
		t.Fatal("staged capture let later tool run")
	}
	engine.structuredSubresult(session.ID, "execution", map[string]any{"call_id": output.CallID, "result": core.ToolResult{Content: "recorded"}})
	if _, ok := handle.Captured(); ok {
		t.Fatal("nested capture committed before outer result")
	}
	engine.structuredTopResult(session.ID, "unrelated-execution", outer.CallID, true)
	if _, ok := handle.Captured(); ok {
		t.Fatal("unrelated outer result committed capture")
	}
	engine.structuredTopResult(session.ID, "execution", outer.CallID, false)
	if _, ok := handle.Captured(); ok || !handle.AcceptsInput() {
		t.Fatal("failed outer result did not reopen input")
	}
}

func TestStructuredOutputPTCRequiresSuccessfulOuterExecution(t *testing.T) {
	for _, mode := range []string{"ptc", "ptc-repl"} {
		t.Run(mode, func(t *testing.T) {
			store, session := setup(t)
			session.Permission, session.ExecutionMode = "full-access", mode
			if err := store.Update(session); err != nil {
				t.Fatal(err)
			}
			registry := tools.New()
			defer registry.Close()
			mutations := 0
			if err := registry.Register(core.ToolSpec{Name: "after_capture_fixture", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, core.ToolContext, json.RawMessage) (core.ToolResult, error) {
				mutations++
				return core.ToolResult{Content: "mutated"}, nil
			}); err != nil {
				t.Fatal(err)
			}
			requests := 0
			model := fakeModel{func(_ context.Context, request core.Request, _ func(core.Delta)) (core.Response, error) {
				requests++
				if len(request.Tools) != 1 || request.Tools[0].Name != "run_code" || !strings.Contains(request.System, "tools.structured_output") {
					t.Fatal("PTC scoped binding was not in ordinary prompt state")
				}
				code := `await tools.structured_output({count:3});await tools.after_capture_fixture({});`
				if requests == 2 {
					if !request.Messages[len(request.Messages)-1].Content[0].IsError {
						t.Fatal("failed outer execution was not visible for retry")
					}
					code = `await tools.structured_output({count:4});`
				} else if requests > 2 {
					t.Fatal("successful structured capture did not conclude the turn")
				}
				return core.Response{Content: []core.Block{{Type: "tool_use", ID: "reused-code-id", Name: "run_code", Input: core.JSON(map[string]any{"code": code})}}}, nil
			}}
			engine := New(store, model, registry, Options{MaxSteps: 3, ExecutionMode: mode})
			registry.SetDispatcher(func(ctx context.Context, tc core.ToolContext, name string, raw json.RawMessage) (core.ToolResult, error) {
				result, handled, err := engine.ExecuteStructured(ctx, tc, name, raw)
				if handled {
					return result, err
				}
				return registry.Execute(ctx, tc, name, raw)
			})
			handle, release, err := engine.AttachStructured(session.ID, structuredCountSchema, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if err := engine.Run(context.Background(), session.ID, core.TextMessage("user", "count")); err != nil {
				t.Fatal(err)
			}
			captured, ok := handle.Captured()
			if !ok || string(captured) != `{"count":4}` || mutations != 0 || requests != 2 {
				t.Fatalf("captured=%s ok=%v mutations=%d requests=%d", captured, ok, mutations, requests)
			}
		})
	}
}

type structuredInboxGate struct {
	*state.Store
	entered chan struct{}
	proceed chan struct{}
}

func (s structuredInboxGate) Append(id, turn, kind string, value any) (core.Event, error) {
	if kind == "user/inbox" {
		close(s.entered)
		<-s.proceed
	}
	return s.Store.Append(id, turn, kind, value)
}

func TestStructuredOutputCannotOvertakeAcceptedInputCommit(t *testing.T) {
	store, session := setup(t)
	gated := structuredInboxGate{Store: store, entered: make(chan struct{}), proceed: make(chan struct{})}
	engine := New(gated, fakeModel{}, &fakeTools{}, Options{})
	handle, release, err := engine.AttachStructured(session.ID, structuredCountSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	defer func() {
		select {
		case <-gated.proceed:
		default:
			close(gated.proceed)
		}
	}()
	inboxDone := make(chan error, 1)
	go func() {
		_, err := engine.Inbox(context.Background(), session.ID, core.TextMessage("user", "accepted input"))
		inboxDone <- err
	}()
	<-gated.entered
	captureDone := make(chan error, 1)
	go func() {
		_, _, err := engine.ExecuteStructured(context.Background(), core.ToolContext{SessionID: session.ID, CallID: "output", ExecutionID: "capture"}, StructuredOutputTool, json.RawMessage(`{"count":3}`))
		captureDone <- err
	}()
	select {
	case err := <-captureDone:
		t.Fatalf("capture crossed input commit boundary: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(gated.proceed)
	if err := <-inboxDone; err != nil {
		t.Fatal(err)
	}
	if err := <-captureDone; err == nil {
		t.Fatal("capture ignored committed pending input")
	}
	if !handle.AcceptsInput() {
		t.Fatal("rejected capture sealed input")
	}
}

func TestStructuredOutputScopesSchemaToOneActivation(t *testing.T) {
	store, session := setup(t)
	other, err := store.Create(t.TempDir(), "other")
	if err != nil {
		t.Fatal(err)
	}
	engine := New(store, fakeModel{}, &fakeTools{}, Options{})
	_, releaseFirst, err := engine.AttachStructured(session.ID, structuredCountSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()
	otherSchema := json.RawMessage(`{"type":"object","properties":{"label":{"type":"string"}},"required":["label"],"additionalProperties":false}`)
	_, releaseOther, err := engine.AttachStructured(other.ID, otherSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseOther()
	for _, pair := range []struct {
		id     string
		schema json.RawMessage
	}{{session.ID, structuredCountSchema}, {other.ID, otherSchema}} {
		spec, ok := engine.StructuredSpec(pair.id)
		if !ok || string(spec.InputSchema) != string(pair.schema) {
			t.Fatal("another activation changed schema")
		}
	}
	if _, _, err := engine.ExecuteStructured(context.Background(), core.ToolContext{SessionID: other.ID, CallID: "output", ExecutionID: "other"}, StructuredOutputTool, json.RawMessage(`{"count":3}`)); err == nil {
		t.Fatal("another activation's schema admitted output")
	}
	releaseFirst()
	if _, ok := engine.StructuredSpec(other.ID); !ok {
		t.Fatal("releasing sibling removed this schema")
	}
}
