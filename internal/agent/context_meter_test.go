package agent

import (
	"context"
	"testing"

	"dsgo/internal/core"
	"dsgo/internal/state"
)

func TestContextEstimateUsesUTF16DensityAndLogicalToolRoles(t *testing.T) {
	for _, tc := range []struct {
		message core.Message
		want    int
	}{
		{core.TextMessage("user", "abc"), 9},
		{core.TextMessage("user", "😀"), 9},
		{core.TextMessage("user", "😀😀😀"), 10},
		{core.TextMessage("system", "abcd"), 5},
		{core.Message{Role: "system"}, 0},
		{core.Message{Role: "assistant", Content: []core.Block{{Type: "tool_use", Name: "read", Input: []byte(`{"x":1}`)}}}, 11},
		{core.Message{Role: "user", Content: []core.Block{{Type: "tool_result", ToolUseID: "a", Content: "one"}, {Type: "tool_result", ToolUseID: "b", Content: "😀x"}}}, 18},
	} {
		got, err := EstimateContextMessage(tc.message)
		if err != nil || got != tc.want {
			t.Fatalf("price %+v: got %d want %d: %v", tc.message, got, tc.want, err)
		}
	}
}

func TestContextPricingUsesEveryImageAndFileProjection(t *testing.T) {
	messages := []core.Message{{Role: "user", Content: []core.Block{
		{Type: "image", Source: map[string]string{"id": "one"}},
		{Type: "tool_result", ToolUseID: "tool", Content: []core.Block{{Type: "image", Source: map[string]string{"id": "two"}}, {Type: "file", Source: map[string]string{"id": "file"}}}},
	}}}
	route := ContextRoute{ImagePricing: func(images []core.Block) ([]ContextImagePrice, error) {
		if len(images) != 2 {
			t.Fatal("image occurrence order was flattened incorrectly", images)
		}
		return []ContextImagePrice{{968, "image"}, {0, "offloaded"}}, nil
	}, FileText: func(core.Block) (string, error) { return "file handle", nil }}
	prices, err := priceContextMessages(messages, route)
	// Two logical roles: original user and tool result. Image handle prices
	// are 6 and 7; file handle is 7, each including its text-block overhead.
	if err != nil || len(prices) != 1 || prices[0] != 968+6+7+7+8 {
		t.Fatalf("request projection price: %+v %v", prices, err)
	}
	route.ImagePricing = func([]core.Block) ([]ContextImagePrice, error) { return nil, nil }
	if _, err := priceContextMessages(messages, route); err == nil {
		t.Fatal("mismatched per-occurrence pricing was accepted")
	}
}

func contextAnchorFixture(t *testing.T, usage core.Usage) (*Engine, *state.Store, string, core.Request) {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(t.TempDir(), "context meter")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(session.ID, "turn", "user/message", core.TextMessage("user", "original")); err != nil {
		t.Fatal(err)
	}
	nodes, err := store.ContextSurface(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	req := core.Request{Provider: "route", Model: "fixture", MaxTokens: 1024, System: "system", Messages: core.ContextMessages(nodes)}
	record, err := contextRequestParts(req, nodes)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []struct {
		kind string
		data any
	}{
		{"context/request", record},
		{"assistant/message", core.TextMessage("assistant", "answer")},
		{"step/usage", usage},
		{"user/message", core.TextMessage("user", "tail")},
	} {
		if _, err := store.Append(session.ID, "turn", event.kind, event.data); err != nil {
			t.Fatal(err)
		}
	}
	req.Messages, err = store.Messages(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	return New(store, nil, nil, Options{ContextTokens: 1000000}), store, session.ID, req
}

func TestContextUsageAnchorRequiresMatchingEnvelopeAndSafeBaseline(t *testing.T) {
	engine, _, id, req := contextAnchorFixture(t, core.Usage{InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 50, CacheWriteTokens: 25})
	measurement, err := engine.MeasureContext(context.Background(), id, req)
	if err != nil || measurement.BaselineKind != "usage" || measurement.BaselineTokens != 1275 || measurement.SurfaceDeltaTokens != 9 || measurement.TotalTokens != 1284 {
		t.Fatalf("usage anchor: %+v %v", measurement, err)
	}
	req.MaxTokens++
	changed, err := engine.MeasureContext(context.Background(), id, req)
	if err != nil || changed.BaselineKind != "estimated" || changed.SurfaceDeltaTokens != 0 || changed.TotalTokens != changed.SurfaceTokens {
		t.Fatalf("changed envelope reused provider usage: %+v %v", changed, err)
	}
	engine, _, id, req = contextAnchorFixture(t, core.Usage{InputTokens: 1})
	low, err := engine.MeasureContext(context.Background(), id, req)
	if err != nil || low.BaselineKind != "estimated" || low.TotalTokens != low.SurfaceTokens {
		t.Fatalf("provider undercount displaced full anchor heuristic: %+v %v", low, err)
	}
}

func TestContextAnchorReplaysAfterDurableReplacementAndRestart(t *testing.T) {
	engine, store, id, req := contextAnchorFixture(t, core.Usage{InputTokens: 1000, OutputTokens: 200})
	nodes, err := store.ContextSurface(id)
	if err != nil {
		t.Fatal(err)
	}
	replacement := core.ContextReplacement{Version: 1, Expected: core.ContextOrigins(nodes), Start: 0, End: 2, Messages: []core.Message{core.TextMessage("user", "x")}}
	if _, err := store.AppendContextReplacement(id, "turn", replacement); err != nil {
		t.Fatal(err)
	}
	req.Messages, err = store.Messages(id)
	if err != nil {
		t.Fatal(err)
	}
	measurement, err := engine.MeasureContext(context.Background(), id, req)
	if err != nil || measurement.BaselineKind != "usage" || measurement.SurfaceDeltaTokens >= 0 {
		t.Fatalf("replacement did not subtract the old surface: %+v %v", measurement, err)
	}
	restarted := New(store, nil, nil, Options{ContextTokens: 1000000})
	again, err := restarted.MeasureContext(context.Background(), id, req)
	if err != nil || again.TotalTokens != measurement.TotalTokens || again.BaselineKind != measurement.BaselineKind {
		t.Fatalf("restart lost anchor/replacement semantics: %+v %v", again, err)
	}
}
