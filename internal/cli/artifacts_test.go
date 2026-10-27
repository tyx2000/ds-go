package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dsgo/internal/artifacts"
	"dsgo/internal/core"
	"dsgo/internal/state"
	"dsgo/internal/tools"
)

func artifactApp(t *testing.T) (*App, *bytes.Buffer) {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(t.TempDir(), "artifacts")
	if err != nil {
		t.Fatal(err)
	}
	output := &bytes.Buffer{}
	a := &App{ctx: context.Background(), store: store, session: session, registry: tools.New(), out: output, err: output}
	if err = a.registerArtifactTools(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.registry.Close() })
	return a, output
}

func TestPresentDurableCardAndFailedAudit(t *testing.T) {
	a, out := artifactApp(t)
	p := filepath.Join(a.session.CWD, "report.txt")
	if err := os.WriteFile(p, []byte("report"), 0600); err != nil {
		t.Fatal(err)
	}
	turn := "turn-fixture"
	if _, err := a.store.Append(a.session.ID, turn, "turn/started", map[string]any{"id": turn}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.Append(a.session.ID, turn, "tool/subcall", map[string]any{"id": "nested-present", "name": "present"}); err != nil {
		t.Fatal(err)
	}
	tc := core.ToolContext{SessionID: a.session.ID, TurnID: turn, CWD: a.session.CWD, Emit: func(kind string, value any) error {
		_, err := a.store.Append(a.session.ID, turn, kind, value)
		return err
	}}
	result, err := a.registry.Execute(context.Background(), tc, "present", core.JSON(map[string]any{"files": []map[string]string{{"path": "report.txt", "description": "final report"}}}))
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		Turn  int
		Files []artifacts.PresentedFile
	}
	canonical, canonicalErr := filepath.EvalSymlinks(p)
	if err = json.Unmarshal([]byte(result.Content), &value); err != nil || canonicalErr != nil || value.Turn != 1 || len(value.Files) != 1 || value.Files[0].Path != canonical {
		t.Fatalf("result %s: %v", result.Content, err)
	}
	if err = a.artifactCommand(nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "deliverables/presented") || !strings.Contains(out.String(), "nested-present") {
		t.Fatal("durable delivery card missing")
	}
	tc.Emit = func(string, any) error { return errors.New("disk full") }
	if _, err = a.registry.Execute(context.Background(), tc, "present", core.JSON(map[string]any{"files": []map[string]string{{"path": p}}})); err == nil {
		t.Fatal("audit failure reported successful delivery")
	}
}

func TestWorkspaceHooksDurableDiffAndDelegatedSkip(t *testing.T) {
	a, out := artifactApp(t)
	turn := "workspace-turn"
	if _, err := a.store.Append(a.session.ID, turn, "turn/started", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.workspaceTurnStarted(context.Background(), a.session, turn); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(a.session.CWD, "new.txt")
	if err := os.WriteFile(p, []byte("new content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.workspaceTurnFinished(context.Background(), a.session, turn); err != nil {
		t.Fatal(err)
	}
	if err := a.artifactCommand([]string{"changes"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"total": 1`) {
		t.Fatalf("summary missing: %s", out.String())
	}
	out.Reset()
	if err := a.artifactCommand([]string{"diff", turn, "0"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "+new content") {
		t.Fatalf("diff missing: %s", out.String())
	}
	delegated := a.session
	delegated.Delegated = true
	if err := a.workspaceTurnStarted(context.Background(), delegated, "child"); err != nil {
		t.Fatal(err)
	}
	if err := a.workspaceTurnFinished(context.Background(), delegated, "child"); err != nil {
		t.Fatal(err)
	}
	s, err := a.artifactStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Changes(a.session.ID, "child"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("delegated snapshot exists: %v", err)
	}
}

func TestSpillDisplayDoesNotMutateCanonicalAndRecoveryCommand(t *testing.T) {
	a, out := artifactApp(t)
	original := core.ToolResult{Content: strings.Repeat("output\n", 12000)}
	var ref artifacts.Spill
	tc := core.ToolContext{SessionID: a.session.ID, TurnID: "turn", Emit: func(kind string, v any) error {
		if kind == "spill/saved" {
			ref = *(v.(*artifacts.Spill))
		}
		return nil
	}}
	retained, err := a.spillToolResult(context.Background(), tc, "run_shell", original)
	if err != nil {
		t.Fatal(err)
	}
	if retained.Content == original.Content || len(retained.Content) > 50000 || len(original.Content) != 84000 {
		t.Fatal("canonical or retained copy is incorrect")
	}
	if err = a.artifactCommand([]string{"spill", ref.ID, "0", "50"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "output") {
		t.Fatal("spill was not recoverable")
	}
}

func TestArtifactOptionalStorageFailureKeepsSuccessfulToolAndTurn(t *testing.T) {
	a, out := artifactApp(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(a.store.Root(), "artifacts")); err != nil {
		t.Fatal(err)
	}
	if err := a.workspaceTurnStarted(context.Background(), a.session, "turn"); err != nil {
		t.Fatalf("optional preview failed the turn: %v", err)
	}
	original := core.ToolResult{Content: strings.Repeat("large output", 10000)}
	result, err := a.spillToolResult(context.Background(), core.ToolContext{SessionID: a.session.ID}, "run_shell", original)
	if err != nil || result.Content != original.Content || result.IsError != original.IsError {
		t.Fatalf("optional spill lost successful content: %v", err)
	}
	if !strings.Contains(out.String(), "artifact preview unavailable") {
		t.Fatal("storage failure was silent")
	}
}

func TestPresentDisplayAndSpillAuditFailure(t *testing.T) {
	a, _ := artifactApp(t)
	canonical := toolValue(map[string]any{"turn": 1, "files": []artifacts.PresentedFile{{Path: "/tmp/report.txt"}}})
	display, err := a.spillToolResult(context.Background(), core.ToolContext{}, "present", canonical)
	if err != nil || display.Content != "Presented /tmp/report.txt" || canonical.Content == display.Content {
		t.Fatalf("present value/display separation: %+v %v", display, err)
	}
	tc := core.ToolContext{SessionID: a.session.ID, Emit: func(string, any) error { return errors.New("audit failure") }}
	if _, err = a.spillToolResult(context.Background(), tc, "run_shell", core.ToolResult{Content: strings.Repeat("x", 60000)}); err == nil {
		t.Fatal("failed spill audit reported success")
	}
}
