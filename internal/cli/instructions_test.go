package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dsgo/internal/agent"
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

func TestInstructionBaselineReturnsAfterVisibleCompactionAndResume(t *testing.T) {
	t.Setenv("DSH_AGENTS_HOME", t.TempDir())
	a, s := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		return core.Response{}, nil
	}))
	if err := os.WriteFile(filepath.Join(s.CWD, "AGENTS.md"), []byte("Keep the exact workspace rule."), 0600); err != nil {
		t.Fatal(err)
	}
	for _, restart := range []bool{false, true} {
		if err := a.ensureInstructionBaseline(a.ctx, s, "turn1"); err != nil {
			t.Fatal(err)
		}
		if _, err := a.store.Append(s.ID, "turn1", "context/compacted", map[string]any{"messages": []core.Message{core.TextMessage("user", "A short history summary."), core.TextMessage("assistant", "Understood.")}}); err != nil {
			t.Fatal(err)
		}
		if got := baselines(t, a, s.ID); len(got) != 0 {
			t.Fatalf("compaction fixture retained a baseline: %q", got)
		}
		if restart {
			a.instructionState = instructionBaselines{}
		}
		if err := a.ensureInstructionBaseline(a.ctx, s, "turn2"); err != nil {
			t.Fatal(err)
		}
		if err := a.ensureInstructionBaseline(a.ctx, s, "turn2"); err != nil {
			t.Fatal(err)
		}
		if got := baselines(t, a, s.ID); len(got) != 1 || !strings.Contains(got[0], "Keep the exact workspace rule.") {
			t.Fatalf("restart=%v visible baseline not restored exactly once: %q", restart, got)
		}
	}
}

func TestInstructionBaselineTracksWorkingDirectory(t *testing.T) {
	t.Setenv("DSH_AGENTS_HOME", t.TempDir())
	a, s := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		return core.Response{}, nil
	}))
	other := t.TempDir()
	for dir, rule := range map[string]string{s.CWD: "Rule for the first project.", other: "Rule for the second project."} {
		if err := os.Mkdir(filepath.Join(dir, ".git"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(rule), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.ensureInstructionBaseline(a.ctx, s, "turn1"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.changeWorkingDirectory(a.ctx, core.ToolContext{SessionID: s.ID}, &other); err != nil {
		t.Fatal(err)
	}
	if err := a.ensureInstructionBaseline(a.ctx, s, "turn1"); err != nil {
		t.Fatal(err)
	}
	if err := a.ensureInstructionBaseline(a.ctx, s, "turn1"); err != nil {
		t.Fatal(err)
	}
	if got := baselines(t, a, s.ID); len(got) != 2 || !strings.Contains(got[1], "replaces all earlier workspace instruction baselines") || !strings.Contains(got[1], "Rule for the second project.") || strings.Contains(got[1], "Rule for the first project.") {
		t.Fatalf("working-directory baseline: %q", got)
	}
}

func TestInstructionBaselineUsesAuthoritativeHarnessHome(t *testing.T) {
	ambient := t.TempDir()
	t.Setenv("DSH_HOME", ambient)
	t.Setenv("DSH_AGENTS_HOME", t.TempDir())
	a, s := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		return core.Response{}, nil
	}))
	for dir, rule := range map[string]string{ambient: "Wrong ambient home rule.", a.store.Root(): "Authoritative harness home rule."} {
		if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(rule), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.ensureInstructionBaseline(a.ctx, s, "turn1"); err != nil {
		t.Fatal(err)
	}
	if got := baselines(t, a, s.ID); len(got) != 1 || !strings.Contains(got[0], "Authoritative harness home rule.") || strings.Contains(got[0], "Wrong ambient home rule.") {
		t.Fatalf("global home mismatch: %q", got)
	}
}

func TestInstructionBaselineIsRestoredInSameRequestAfterAutomaticCompaction(t *testing.T) {
	t.Setenv("DSH_AGENTS_HOME", t.TempDir())
	var compacted int
	var sent []core.Message
	a, s := lifecycleApp(t, lifecycleModel(func(_ context.Context, req core.Request, _ func(core.Delta)) (core.Response, error) {
		if req.Purpose == "compaction" {
			compacted++
			return core.Response{Content: []core.Block{{Type: "text", Text: "The earlier discussion is summarized here."}}}, nil
		}
		sent = req.Messages
		return core.Response{Content: []core.Block{{Type: "text", Text: "Done."}}}, nil
	}))
	const rule = "Keep the exact rule after automatic compaction."
	if err := os.WriteFile(filepath.Join(s.CWD, "AGENTS.md"), []byte(rule), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.ensureInstructionBaseline(a.ctx, s, "earlier"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		turn := agent.ID()
		for _, event := range []struct {
			kind string
			data any
		}{
			{"turn/started", map[string]any{"id": turn}},
			{"user/message", core.TextMessage("user", strings.Repeat("u", 2000))},
			{"assistant/message", core.TextMessage("assistant", strings.Repeat("a", 2000))},
			{"turn/finished", map[string]any{"status": "completed"}},
		} {
			if _, err := a.store.Append(s.ID, turn, event.kind, event.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	a.engine.UpdateOptions(func(o *agent.Options) {
		o.ContextTokens, o.MaxTokens = 4000, 100
		o.BeforeStep = func(ctx context.Context, s core.Session, turn string, _ int) error {
			return a.ensureInstructionBaseline(ctx, s, turn)
		}
		o.AfterCompaction = a.ensureInstructionBaseline
	})
	if err := a.engine.Run(a.ctx, s.ID, core.TextMessage("user", "Continue now.")); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, message := range sent {
		if messageSourceKind(message) == "agent-instructions" {
			count++
			if len(message.Content) != 1 || !strings.Contains(message.Content[0].Text, rule) {
				t.Fatalf("wrong instruction restored: %+v", message)
			}
		}
	}
	if compacted == 0 || count != 1 {
		t.Fatalf("same final request missed restored baseline: compactions=%d baselines=%d", compacted, count)
	}
}
