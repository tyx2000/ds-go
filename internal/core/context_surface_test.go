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

func TestLegacyCompactedTailRetainsRealGroupsAndCanPruneToolResult(t *testing.T) {
	for _, hasHistory := range []bool{true, false} {
		direct := TextMessage("user", "direct")
		capture := TextMessage("user", "capture")
		capture.Source = map[string]any{"kind": "file-reference"}
		assistant := Message{Role: "assistant", Content: []Block{{Type: "tool_use", ID: "call", Name: "read", Input: JSON(map[string]any{})}}}
		result := Message{Role: "user", Content: []Block{{Type: "tool_result", ToolUseID: "call", Content: "large result"}}}
		next := TextMessage("user", "next input")
		events := []Event{}
		if hasHistory {
			events = []Event{{Seq: 1, Type: "user/input", Data: JSON(InputBatch{1, direct, []Message{capture}})}, {Seq: 2, Type: "assistant/message", Data: JSON(assistant)}, {Seq: 3, Type: "tool/result", Data: JSON(result)}, {Seq: 4, Type: "user/message", Data: JSON(next)}}
		}
		messages := []Message{TextMessage("user", "legacy summary"), TextMessage("assistant", "legacy ack"), direct, capture, assistant, result, next}
		events = append(events, Event{Seq: 5, Type: "context/compacted", Data: JSON(map[string]any{"messages": messages, "retained_input_start": 2})})
		nodes, err := ContextSurface(events)
		if err != nil {
			t.Fatal(err)
		}
		if nodes[2].InputGroup == 0 || nodes[2].InputGroup != nodes[3].InputGroup || nodes[4].InputGroup != 0 || nodes[5].InputGroup != 0 || nodes[6].InputGroup == nodes[2].InputGroup {
			t.Fatalf("legacy tail groups conflated: %#v", nodes)
		}
		if hasHistory && nodes[2].InputGroup != 1 {
			t.Fatal("original atomic input membership was lost")
		}
		for i, node := range nodes {
			if node.Origin.Seq != 5 || node.Origin.Part != i {
				t.Fatal("legacy replay changed persisted CAS origin")
			}
		}
		pruned := result
		pruned.Content = []Block{{Type: "tool_result", ToolUseID: "call", Content: "small result"}}
		replacement := ContextReplacement{1, ContextOrigins(nodes), 5, 6, []Message{pruned}}
		events = append(events, Event{Seq: 6, Type: "context/replace", Data: JSON(replacement)})
		if _, err = ContextSurface(events); err != nil {
			t.Fatal("independent retained result cannot be replaced", err)
		}
	}
}
