package state

import (
	"strings"
	"testing"

	"dsgo/internal/core"
)

func TestMarkdownExportKeepsBatchedUserTask(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Create(t.TempDir(), "export")
	if err != nil {
		t.Fatal(err)
	}
	task := core.TextMessage("user", "UNIQUE_USER_TASK with @reference")
	snapshot := core.TextMessage("user", "CAPTURED_REFERENCE_SNAPSHOT")
	snapshot.Source = map[string]any{"kind": "session-reference"}
	if _, err := store.AppendInputBatch(s.ID, "turn", task, []core.Message{snapshot}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(s.ID, "turn", "assistant/message", core.TextMessage("assistant", "ASSISTANT_ONLY")); err != nil {
		t.Fatal(err)
	}
	output, err := store.Export(s.ID, "markdown")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), "UNIQUE_USER_TASK") || !strings.Contains(string(output), "CAPTURED_REFERENCE_SNAPSHOT") || !strings.Contains(string(output), "captured context") {
		t.Fatalf("Markdown export silently lost user task: %s", output)
	}
}

func TestMarkdownExportPreservesBothInputGenerationsAndRawEvents(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Create(t.TempDir(), "export")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendInputBatch(s.ID, "first", core.TextMessage("user", "OLD_GENERATION"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendInputBatch(s.ID, "second", core.TextMessage("user", "NEW_GENERATION"), []core.Message{core.TextMessage("user", "CAPTURE_ONLY")}); err != nil {
		t.Fatal(err)
	}
	before, err := store.Export(s.ID, "jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"markdown", "md"} {
		output, err := store.Export(s.ID, format)
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range []string{"OLD_GENERATION", "NEW_GENERATION", "CAPTURE_ONLY"} {
			if strings.Count(string(output), text) != 1 {
				t.Fatalf("%s duplicated or omitted: %s", text, output)
			}
		}
	}
	after, err := store.Export(s.ID, "jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("Markdown export modified raw events")
	}
}
