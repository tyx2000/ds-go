package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	instructions "dsgo/internal/context"
	"dsgo/internal/core"
)

func baselines(t *testing.T, a *App, id string) []string {
	t.Helper()
	messages, err := a.store.Messages(id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, message := range messages {
		if messageSourceKind(message) == "agent-instructions" {
			out = append(out, message.Content[0].Text)
		}
	}
	return out
}

func TestInstructionBaselineIsDurableAndReconciledOncePerProcess(t *testing.T) {
	t.Setenv("DSH_HOME", t.TempDir())
	t.Setenv("DSH_AGENTS_HOME", t.TempDir())
	a, s := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		return core.Response{}, nil
	}))
	if err := os.Mkdir(filepath.Join(s.CWD, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	rules := filepath.Join(s.CWD, "AGENTS.md")
	if err := os.WriteFile(rules, []byte("Use tabs.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	step := func() {
		t.Helper()
		if err := a.prepareStep(a.ctx, s, "turn1", 1); err != nil {
			t.Fatal(err)
		}
	}
	step()
	step()
	got := baselines(t, a, s.ID)
	if len(got) != 1 || !strings.HasPrefix(got[0], "<system-reminder>\nThe following workspace instructions may be relevant") || !strings.Contains(got[0], "Instructions from: AGENTS.md") || !strings.Contains(got[0], "Use tabs.") {
		t.Fatalf("baseline: %q", got)
	}
	// A new process reuses the compatible baseline instead of re-sending it.
	a.instructionState = instructionBaselines{}
	step()
	if got = baselines(t, a, s.ID); len(got) != 1 {
		t.Fatalf("unchanged chain re-sent on resume: %d", len(got))
	}
	// A changed file produces one complete replacement on the next process.
	if err := os.WriteFile(rules, []byte("Use spaces.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a.instructionState = instructionBaselines{}
	step()
	a.instructionState = instructionBaselines{}
	step()
	got = baselines(t, a, s.ID)
	if len(got) != 2 || !strings.Contains(got[1], "replaces all earlier workspace instruction baselines") || !strings.Contains(got[1], "Use spaces.") {
		t.Fatalf("replacement: %q", got)
	}
	// Removing every file replaces the baseline with an empty one, once.
	if err := os.Remove(rules); err != nil {
		t.Fatal(err)
	}
	a.instructionState = instructionBaselines{}
	step()
	a.instructionState = instructionBaselines{}
	step()
	got = baselines(t, a, s.ID)
	if len(got) != 3 || !strings.Contains(got[2], "No workspace instructions are currently active.") {
		t.Fatalf("empty replacement: %q", got)
	}
}

func TestInstructionBaselineSkipsMinimalAndLeavesSystemPrompt(t *testing.T) {
	t.Setenv("DSH_HOME", t.TempDir())
	t.Setenv("DSH_AGENTS_HOME", t.TempDir())
	a, s := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		return core.Response{}, nil
	}))
	if err := os.WriteFile(filepath.Join(s.CWD, "AGENTS.md"), []byte("Project rule.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s.Preset = "minimal"
	if err := a.prepareStep(a.ctx, s, "turn1", 1); err != nil {
		t.Fatal(err)
	}
	if got := baselines(t, a, s.ID); len(got) != 0 {
		t.Fatalf("minimal preset received a baseline: %q", got)
	}
	system, err := instructions.System(s.CWD, nil)
	if err != nil || strings.Contains(system, "Project rule.") {
		t.Fatalf("AGENTS.md content still in the system prompt: %v", err)
	}
}
