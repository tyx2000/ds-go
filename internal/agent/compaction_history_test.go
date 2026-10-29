package agent

import (
	"context"
	"strings"
	"testing"

	"dsgo/internal/core"
)

func TestCompactionNeverInterpolatesOpaqueReplayIntoPrompt(t *testing.T) {
	store, session := setup(t)
	prior := core.Message{Role: "assistant", ReplayModel: "old-model", ReplayProvider: "old-provider", ReplayIdentity: "private-route-tag", Source: map[string]string{"opaque": "opaque-source"}, Content: []core.Block{
		{Type: "thinking", Thinking: "visible thought", Signature: "opaque-signature", Source: map[string]string{"encrypted_content": "opaque-encrypted"}},
		{Type: "redacted_thinking", Data: "opaque-redacted"},
		{Type: "text", Text: "visible answer"},
	}}
	for _, message := range []core.Message{core.TextMessage("user", "first"), prior, core.TextMessage("user", "recent")} {
		if _, err := store.Append(session.ID, "turn", message.Role+"/message", message); err != nil {
			t.Fatal(err)
		}
	}
	model := fakeModel{func(_ context.Context, request core.Request, _ func(core.Delta)) (core.Response, error) {
		prompt := request.Messages[0].Content[0].Text
		for _, forbidden := range []string{"opaque-", "private-route-tag", "replay_identity", "replay_model", "replay_provider"} {
			if strings.Contains(prompt, forbidden) {
				t.Fatalf("compaction prompt contains %q", forbidden)
			}
		}
		if !strings.Contains(prompt, "visible thought") || !strings.Contains(prompt, "visible answer") {
			t.Fatal("compaction lost neutral content")
		}
		return core.Response{Content: []core.Block{{Type: "text", Text: "summary"}}}, nil
	}}
	engine := New(store, model, &fakeTools{}, Options{})
	if err := engine.Compact(context.Background(), session.ID); err != nil {
		t.Fatal(err)
	}
	// Compaction projection does not rewrite the original provider-private audit.
	events, err := store.Events(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, event := range events {
		if event.Type == "assistant/message" {
			found = strings.Contains(string(event.Data), "opaque-signature") && strings.Contains(string(event.Data), "private-route-tag")
		}
	}
	if !found {
		t.Fatal("compaction mutated durable replay")
	}
}
