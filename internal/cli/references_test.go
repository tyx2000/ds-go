package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dsgo/internal/agent"
	"dsgo/internal/core"
	"dsgo/internal/reference"
	"dsgo/internal/state"
)

func TestCLICompletionSelectsPathsAndSessionsWithoutReadingContents(t *testing.T) {
	a, parent := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		return core.Response{Content: []core.Block{{Type: "text", Text: "done"}}}, nil
	}))
	t.Cleanup(a.closeFileSearches)
	if err := os.WriteFile(filepath.Join(parent.CWD, "source with spaces.go"), []byte("file contents stay behind read"), 0600); err != nil {
		t.Fatal(err)
	}
	source, _ := a.store.Create(parent.CWD, "renamed session")
	source.CreationLabel = "original label"
	if err := a.store.Update(source); err != nil {
		t.Fatal(err)
	}
	complete := a.completionFor(parent.ID)
	slash := complete("/pre", 4)
	found := false
	for _, candidate := range slash.Candidates {
		found = found || candidate.Value == "/preview "
	}
	if !found {
		t.Fatal("slash completion missing preview", slash)
	}
	line := `请看 @"source`
	files := complete(line, len([]rune(line)))
	found = false
	for _, candidate := range files.Candidates {
		found = found || candidate.Value == `@"source with spaces.go"`
		if strings.Contains(candidate.Value, "contents stay") {
			t.Fatal("file completion attached its content")
		}
	}
	if !found || files.Start != 3 {
		t.Fatalf("quoted CJK completion %+v", files)
	}
	sessions := complete("@original", 9)
	found = false
	for _, candidate := range sessions.Candidates {
		found = found || candidate.Value == reference.FormatSessionMention(source.ID, "original label")
	}
	if !found {
		t.Fatalf("session completion lacks cold creation label: %+v", sessions)
	}
}

func TestCLIReferenceCaptureIsDurableAndNotRefreshedOnResume(t *testing.T) {
	requests := make(chan core.Request, 3)
	a, parent := lifecycleApp(t, lifecycleModel(func(_ context.Context, request core.Request, _ func(core.Delta)) (core.Response, error) {
		requests <- request
		return core.Response{Content: []core.Block{{Type: "text", Text: "done"}}}, nil
	}))
	a.engine.UpdateOptions(func(options *agent.Options) { options.PrepareInput = a.prepareInput })
	source, _ := a.store.Create(parent.CWD, "source")
	_, _ = a.store.Append(source.ID, "source-turn", "user/message", core.TextMessage("user", "original captured text"))
	input := core.TextMessage("user", "Compare "+reference.FormatSessionMention(source.ID, "source"))
	input.DeliveryID = "input:reference"
	if err := a.engine.Run(a.ctx, parent.ID, input); err != nil {
		t.Fatal(err)
	}
	first := <-requests
	if len(first.Messages) != 2 || first.Messages[0].Content[0].Text != "Compare @source" || !strings.Contains(first.Messages[1].Content[0].Text, "original captured text") {
		t.Fatalf("wrong model projection %+v", first.Messages)
	}
	if !a.inputAdmitted(parent.ID, input.DeliveryID) {
		t.Fatal("batch admission not recognized")
	}
	_, _ = a.store.Append(source.ID, "later", "assistant/message", core.TextMessage("assistant", "later source mutation"))
	if err := a.engine.Run(a.ctx, parent.ID, core.TextMessage("user", "continue")); err != nil {
		t.Fatal(err)
	}
	second := <-requests
	if strings.Contains(string(core.JSON(second.Messages)), "later source mutation") {
		t.Fatal("replay recaptured source")
	}
	events, _ := a.store.Events(parent.ID)
	count := 0
	for _, event := range events {
		if event.Type == "user/input" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("snapshot recaptured %d times", count)
	}
}

func TestCLIQueuedReferenceCapturesAtAdmissionAndSpillsWithSource(t *testing.T) {
	requests := make(chan core.Request, 1)
	a, parent := lifecycleApp(t, lifecycleModel(func(_ context.Context, request core.Request, _ func(core.Delta)) (core.Response, error) {
		requests <- request
		return core.Response{Content: []core.Block{{Type: "text", Text: "done"}}}, nil
	}))
	a.engine.UpdateOptions(func(options *agent.Options) { options.PrepareInput = a.prepareInput })
	limit := 1024
	a.cfg.SessionReferences = &state.SessionReferenceSettings{MaxReferenceBytes: &limit}
	source, _ := a.store.Create(parent.CWD, "source")
	_, _ = a.store.Append(source.ID, "source", "user/message", core.TextMessage("user", "before queue"))
	if _, err := a.engine.Inbox(a.ctx, parent.ID, core.TextMessage("user", "Compare "+reference.FormatSessionMention(source.ID, "source"))); err != nil {
		t.Fatal(err)
	}
	_, _ = a.store.Append(source.ID, "source", "assistant/message", core.TextMessage("assistant", strings.Repeat("long captured data ", 1000)+" admission version"))
	if err := a.engine.Continue(a.ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	request := <-requests
	contextText := request.Messages[1].Content[0].Text
	if !strings.Contains(contextText, "admission version") || !strings.Contains(contextText, `"status":"saved"`) {
		t.Fatalf("capture or spill missing: %s", contextText)
	}
	paths, err := filepath.Glob(filepath.Join(a.store.Root(), "artifacts", "spills", "*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("spill metadata %v %v", paths, err)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		Tool, TurnID string
		Source       struct {
			Kind      string
			SessionID string `json:"session_id"`
		}
	}
	if err = json.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Tool != "" || metadata.TurnID != "" || metadata.Source.Kind != "session-reference" || metadata.Source.SessionID != source.ID {
		t.Fatalf("spill has fabricated tool attribution: %s", raw)
	}
}
