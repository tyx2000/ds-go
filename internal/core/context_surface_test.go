package core

import (
	"strings"
	"testing"
)

func TestContextSurfaceKeepsInputBatchAndNativeReplacement(t *testing.T) {
	direct := TextMessage("user", "request")
	direct.DeliveryID = "delivery"
	snapshot := TextMessage("user", "captured source")
	snapshot.Source = map[string]any{"kind": "session-reference"}
	events := []Event{
		{Seq: 1, Type: "user/input", Data: JSON(InputBatch{1, direct, []Message{snapshot}})},
		{Seq: 2, Type: "assistant/message", Data: JSON(TextMessage("assistant", "answer"))},
	}
	nodes, err := ContextSurface(events)
	if err != nil || len(nodes) != 3 || nodes[0].InputGroup != nodes[1].InputGroup || nodes[0].InputGroup == 0 {
		t.Fatalf("batch projection: %+v %v", nodes, err)
	}
	replacement := ContextReplacement{1, ContextOrigins(nodes), 0, 1, []Message{TextMessage("user", "summary")}}
	if err := ValidateContextReplacement(nodes, replacement); err == nil {
		t.Fatal("replacement split the direct input from its source captures")
	}
	replacement.End = 2
	replacement.Messages[0].Source = map[string]any{"kind": "compact-checkpoint"}
	events = append(events, Event{Seq: 3, Type: "context/replace", Data: JSON(replacement)})
	next, err := ContextSurface(events)
	if err != nil || len(next) != 2 || next[0].Origin.Seq != 3 || next[1].Origin != nodes[2].Origin || next[0].InputGroup != 0 {
		t.Fatalf("replacement projection: %+v %v", next, err)
	}
	if nodes[0].Message.DeliveryID != "delivery" || nodes[1].Message.Content[0].Text != "captured source" {
		t.Fatal("projection edited its canonical input")
	}
	events = append(events, Event{Seq: 4, Type: "context/replace", Data: JSON(replacement)})
	if _, err := ContextSurface(events); err == nil || !strings.Contains(err.Error(), "surface") {
		t.Fatalf("stale replacement accepted on replay: %v", err)
	}
}

func TestContextSurfaceGeneratedSnapshotsAreNotNewInputs(t *testing.T) {
	for _, kind := range []string{"plan-mode", "time-context", "agent-instructions", "compact-checkpoint", "session-reference", "file-reference"} {
		message := TextMessage("user", "generated")
		message.Source = map[string]any{"kind": kind}
		nodes, err := ContextSurface([]Event{{Seq: 1, Type: "user/message", Data: JSON(message)}})
		if err != nil || len(nodes) != 1 || nodes[0].InputGroup != 0 {
			t.Fatalf("generated %s gained input authority: %+v %v", kind, nodes, err)
		}
	}
	for _, kind := range []string{"user", "schedule", "goal", "agent", "tool-jobs"} {
		message := TextMessage("user", "accepted input")
		message.Source = map[string]any{"kind": kind}
		nodes, err := ContextSurface([]Event{{Seq: 1, Type: "user/message", Data: JSON(message)}})
		if err != nil || nodes[0].InputGroup != 1 {
			t.Fatalf("accepted %s lost input identity: %+v %v", kind, nodes, err)
		}
	}
}

func TestContextReplacementRequiresStrictNativeShape(t *testing.T) {
	for _, raw := range []string{
		`{"version":1,"start":0,"end":1,"messages":[]}`,
		`{"version":1,"expected":null,"start":0,"end":1,"messages":[]}`,
		`{"version":1,"expected":[],"start":null,"end":1,"messages":[]}`,
		`{"version":1,"expected":[],"start":0,"end":1,"messages":[],"unknown":true}`,
	} {
		if _, err := ContextSurface([]Event{{Seq: 1, Type: "context/replace", Data: []byte(raw)}}); err == nil {
			t.Fatalf("malformed replacement accepted: %s", raw)
		}
	}
}
