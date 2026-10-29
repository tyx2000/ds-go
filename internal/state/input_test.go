package state

import (
	"os"
	"path/filepath"
	"testing"

	"dsgo/internal/core"
)

func TestCapturedInputCommitsProjectsAndForksAsOneGroup(t *testing.T) {
	s, _ := Open(t.TempDir())
	session, _ := s.Create(t.TempDir(), "references")
	_, _ = s.Append(session.ID, "prior", "user/message", core.TextMessage("user", "prior"))
	_, _ = s.Append(session.ID, "prior", "assistant/message", core.TextMessage("assistant", "answer"))
	direct := core.TextMessage("user", "read @source")
	direct.DeliveryID = "inbox:accepted"
	context := core.TextMessage("user", "immutable captured source")
	context.Source = map[string]any{"kind": "session-reference"}
	event, err := s.AppendInputBatch(session.ID, "now", direct, []core.Message{context})
	if err != nil || event.Type != "user/input" {
		t.Fatalf("input commit %+v %v", event, err)
	}
	assert := func(id string) {
		t.Helper()
		messages, err := s.Messages(id)
		if err != nil || len(messages) != 4 || messages[2].DeliveryID != direct.DeliveryID || messages[3].Content[0].Text != context.Content[0].Text {
			t.Fatalf("projection %v %v", messages, err)
		}
		start, err := s.CompactionBoundary(id)
		if err != nil || start != 2 {
			t.Fatalf("compaction splits input group: %d %v", start, err)
		}
	}
	assert(session.ID)
	fork, err := s.Fork(session.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	assert(fork.ID)
	_, err = s.Append(session.ID, "compact", "context/compacted", map[string]any{"messages": []core.Message{core.TextMessage("user", "summary"), core.TextMessage("assistant", "ack"), direct, context}, "retained_input_start": 2})
	if err != nil {
		t.Fatal(err)
	}
	assert(session.ID)
}

func TestTornCapturedInputPublishesNeitherPromptNorSnapshot(t *testing.T) {
	s, _ := Open(t.TempDir())
	session, _ := s.Create(t.TempDir(), "references")
	_, _ = s.Append(session.ID, "prior", "user/message", core.TextMessage("user", "prior"))
	path := filepath.Join(s.Root(), "sessions", session.ID, "events.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.AppendInputBatch(session.ID, "next", core.TextMessage("user", "new prompt"), []core.Message{core.TextMessage("user", "snapshot")})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Truncate(path, int64(len(before)+12)); err != nil {
		t.Fatal(err)
	}
	reopened, _ := Open(s.Root())
	messages, err := reopened.Messages(session.ID)
	if err != nil || len(messages) != 1 || messages[0].Content[0].Text != "prior" {
		t.Fatalf("partial group published: %+v %v", messages, err)
	}
}
