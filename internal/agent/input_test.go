package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dsgo/internal/core"
	"dsgo/internal/state"
)

func inputSnapshot(text string) core.Message {
	message := core.TextMessage("user", text)
	message.Source = map[string]any{"kind": "session-reference", "version": 1}
	return message
}

func TestPrepareInputCapturesAtInboxAdmissionAndNeverRecursively(t *testing.T) {
	store, session := setup(t)
	version, prepared, calls := "queued", 0, 0
	model := fakeModel{func(_ context.Context, request core.Request, _ func(core.Delta)) (core.Response, error) {
		calls++
		if len(request.Messages) < 2 || request.Messages[0].Content[0].Text != "direct @source" || request.Messages[1].Content[0].Text != "snapshot admitted @nested" {
			t.Fatalf("input/snapshot not adjacent and stable: %+v", request.Messages)
		}
		return core.Response{Content: []core.Block{{Type: "text", Text: "done"}}}, nil
	}}
	opts := Options{PrepareInput: func(_ context.Context, seen core.Session, input core.Message) (core.Message, []core.Message, error) {
		prepared++
		if seen.ID != session.ID || input.Content[0].Text != "direct @source" {
			t.Fatal("snapshot was recursively prepared")
		}
		return input, []core.Message{inputSnapshot("snapshot " + version + " @nested")}, nil
	}}
	engine := New(store, model, &fakeTools{}, opts)
	if _, err := engine.Inbox(context.Background(), session.ID, core.TextMessage("user", "direct @source")); err != nil {
		t.Fatal(err)
	}
	if prepared != 0 {
		t.Fatal("queued input captured source before admission")
	}
	version = "admitted"
	if err := engine.ContinueInbox(context.Background(), session.ID); err != nil {
		t.Fatal(err)
	}
	if pending, err := engine.PendingInbox(session.ID); err != nil || pending {
		t.Fatal("batch did not consume inbox", pending, err)
	}
	version = "changed after admission"
	if err := engine.ContinueInbox(context.Background(), session.ID); err != nil {
		t.Fatal(err)
	}
	if prepared != 1 || calls != 1 {
		t.Fatalf("prepared=%d model=%d", prepared, calls)
	}
	// A new process replays the captured bytes; it never prepares a stored
	// sourced context or reopens the source session.
	restarted := New(store, model, &fakeTools{}, opts)
	if err := restarted.Continue(context.Background(), session.ID); err != nil {
		t.Fatal(err)
	}
	if prepared != 1 {
		t.Fatal("restart recaptured source")
	}
}

type committedInputFailure struct {
	*state.Store
	failed bool
}

func (s *committedInputFailure) AppendInputBatch(id, turn string, message core.Message, contexts []core.Message) (core.Event, error) {
	event, err := s.Store.AppendInputBatch(id, turn, message, contexts)
	if err == nil && !s.failed {
		s.failed = true
		return event, errors.New("simulated interruption after input batch commit")
	}
	return event, err
}

func TestPrepareInputCommittedBatchSurvivesFailureWithoutRecapture(t *testing.T) {
	store, session := setup(t)
	prepared, calls, version := 0, 0, "original"
	opts := Options{PrepareInput: func(_ context.Context, _ core.Session, input core.Message) (core.Message, []core.Message, error) {
		prepared++
		return input, []core.Message{inputSnapshot("snapshot " + version)}, nil
	}}
	model := fakeModel{func(_ context.Context, request core.Request, _ func(core.Delta)) (core.Response, error) {
		calls++
		if len(request.Messages) != 2 || request.Messages[1].Content[0].Text != "snapshot original" {
			t.Fatalf("committed snapshot lost: %+v", request.Messages)
		}
		return core.Response{Content: []core.Block{{Type: "text", Text: "done"}}}, nil
	}}
	input := core.TextMessage("user", "direct")
	input.DeliveryID = "delivery-original"
	engine := New(&committedInputFailure{Store: store}, model, &fakeTools{}, opts)
	if err := engine.Run(context.Background(), session.ID, input); err == nil {
		t.Fatal("missing simulated interruption")
	}
	version = "new source version"
	restarted := New(store, model, &fakeTools{}, opts)
	if err := restarted.Run(context.Background(), session.ID, input); err != nil {
		t.Fatal(err)
	}
	if prepared != 1 || calls != 0 {
		t.Fatal("duplicate input was recaptured or run", prepared, calls)
	}
	if err := restarted.Continue(context.Background(), session.ID); err != nil {
		t.Fatal(err)
	}
	if prepared != 1 || calls != 1 {
		t.Fatal("recovery recaptured source", prepared, calls)
	}
	events, err := store.Events(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	batches := 0
	for _, event := range events {
		if event.Type == "user/input" {
			batches++
		}
	}
	if batches != 1 {
		t.Fatal("input batch duplicated", batches)
	}
}

type baseInputStore struct{ Store }

func TestPrepareInputLegacyStorePreservesOldPathAndRefusesPartialBatch(t *testing.T) {
	for _, contexts := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "context"}[contexts], func(t *testing.T) {
			store, session := setup(t)
			model := fakeModel{func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
				return core.Response{Content: []core.Block{{Type: "text", Text: "done"}}}, nil
			}}
			engine := New(baseInputStore{store}, model, &fakeTools{}, Options{PrepareInput: func(_ context.Context, _ core.Session, input core.Message) (core.Message, []core.Message, error) {
				if contexts {
					return input, []core.Message{inputSnapshot("snapshot")}, nil
				}
				return input, nil, nil
			}})
			err := engine.Run(context.Background(), session.ID, core.TextMessage("user", "direct"))
			messages, readErr := store.Messages(session.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if contexts {
				if err == nil || !strings.Contains(err.Error(), "atomic input-batch") || len(messages) != 0 {
					t.Fatal("partial input committed", err, messages)
				}
			} else if err != nil || len(messages) != 2 {
				t.Fatal("legacy input failed", err, messages)
			}
		})
	}
}

func TestPrepareInputCancellationDoesNotCommitInput(t *testing.T) {
	store, session := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine := New(store, fakeModel{}, &fakeTools{}, Options{PrepareInput: func(_ context.Context, _ core.Session, input core.Message) (core.Message, []core.Message, error) {
		cancel()
		return input, []core.Message{inputSnapshot("snapshot")}, nil
	}})
	if err := engine.Run(ctx, session.ID, core.TextMessage("user", "direct")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	messages, err := store.Messages(session.ID)
	if err != nil || len(messages) != 0 {
		t.Fatal("canceled preparation committed input", err, messages)
	}
}

func TestCompactionKeepsDirectInputAndSnapshotsTogether(t *testing.T) {
	store, session := setup(t)
	for _, message := range []core.Message{core.TextMessage("user", "old direct"), core.TextMessage("assistant", "old answer")} {
		if _, err := store.Append(session.ID, "old", message.Role+"/message", message); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.AppendInputBatch(session.ID, "latest", core.TextMessage("user", "latest direct"), []core.Message{inputSnapshot("latest snapshot")}); err != nil {
		t.Fatal(err)
	}
	model := fakeModel{func(_ context.Context, request core.Request, _ func(core.Delta)) (core.Response, error) {
		prompt := request.Messages[0].Content[0].Text
		if strings.Contains(prompt, "latest direct") || strings.Contains(prompt, "latest snapshot") {
			t.Fatal("compaction split latest batch")
		}
		return core.Response{Content: []core.Block{{Type: "text", Text: "summary"}}}, nil
	}}
	engine := New(store, model, &fakeTools{}, Options{})
	if err := engine.Compact(context.Background(), session.ID); err != nil {
		t.Fatal(err)
	}
	messages, err := store.Messages(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 || messages[2].Content[0].Text != "latest direct" || messages[3].Content[0].Text != "latest snapshot" {
		t.Fatal("latest input batch detached", messages)
	}
	if boundary, err := store.CompactionBoundary(session.ID); err != nil || boundary != 2 {
		t.Fatal("compaction boundary not retained", boundary, err)
	}
}
