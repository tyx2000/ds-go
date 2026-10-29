package state

import (
	"sync"
	"sync/atomic"
	"testing"

	"dsgo/internal/core"
)

func TestContextReplacementCASIsAtomicAcrossWritersAndRestart(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(t.TempDir(), "context")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Append(session.ID, "", "user/message", core.TextMessage("user", "original")); err != nil {
		t.Fatal(err)
	}
	nodes, err := store.ContextSurface(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	replacement := core.ContextReplacement{Version: 1, Expected: core.ContextOrigins(nodes), Start: 0, End: 1, Messages: []core.Message{core.TextMessage("user", "summary")}}
	var successes atomic.Int32
	var writers sync.WaitGroup
	for range 2 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			if _, err := store.AppendContextReplacement(session.ID, "", replacement); err == nil {
				successes.Add(1)
			}
		}()
	}
	writers.Wait()
	if successes.Load() != 1 {
		t.Fatalf("CAS successes=%d", successes.Load())
	}
	reopened, err := Open(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	messages, err := reopened.Messages(session.ID)
	if err != nil || len(messages) != 1 || messages[0].Content[0].Text != "summary" {
		t.Fatalf("restart projection: %+v %v", messages, err)
	}
	events, err := reopened.Events(session.ID)
	if err != nil || len(events) != 2 {
		t.Fatalf("failed writer left a partial event: %+v %v", events, err)
	}
}

func TestContextReplacementPreservesBatchBoundaryAndGeneratedContext(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(t.TempDir(), "batch")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := core.TextMessage("user", "snapshot")
	snapshot.Source = map[string]any{"kind": "file-reference"}
	if _, err = store.AppendInputBatch(session.ID, "", core.TextMessage("user", "direct"), []core.Message{snapshot}); err != nil {
		t.Fatal(err)
	}
	generated := core.TextMessage("user", "time snapshot")
	generated.Source = map[string]any{"kind": "time-context", "form": "snapshot"}
	if _, err = store.Append(session.ID, "", "user/message", generated); err != nil {
		t.Fatal(err)
	}
	if boundary, err := store.CompactionBoundary(session.ID); err != nil || boundary != 0 {
		t.Fatalf("generated snapshot stole direct boundary: %d %v", boundary, err)
	}
	nodes, err := store.ContextSurface(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	replacement := core.ContextReplacement{Version: 1, Expected: core.ContextOrigins(nodes), Start: 1, End: 2, Messages: []core.Message{generated}}
	if _, err := store.AppendContextReplacement(session.ID, "", replacement); err == nil {
		t.Fatal("atomic input batch was split")
	}
	if events, err := store.Events(session.ID); err != nil || len(events) != 2 {
		t.Fatal("invalid replacement appended a fact", events, err)
	}
}
