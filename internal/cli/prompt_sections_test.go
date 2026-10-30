package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dsgo/internal/agent"
	"dsgo/internal/core"
)

func TestSystemForUsesOrderedPresetInActualRequestAndPreservesSkills(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var sent string
	a, s := lifecycleApp(t, lifecycleModel(func(_ context.Context, req core.Request, _ func(core.Delta)) (core.Response, error) {
		sent = req.System
		return core.Response{Content: []core.Block{{Type: "text", Text: "Done."}}}, nil
	}))
	s.Preset, s.ExecutionMode = "standard", "native"
	if err := a.store.Update(s); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(s.CWD, ".agents", "skills", "catalog-fixture")
	if err := os.MkdirAll(skillDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: catalog-fixture\ndescription: A preserved catalog entry\n---\nLoad this body on demand.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.setPlan(s.ID, true); err != nil {
		t.Fatal(err)
	}
	a.engine.UpdateOptions(func(o *agent.Options) { o.SystemFor = a.systemFor })
	if err := a.engine.Run(a.ctx, s.ID, core.TextMessage("user", "Inspect the request policy.")); err != nil {
		t.Fatal(err)
	}
	identity := strings.Index(sent, "You are an AI agent powered by DeepSeek Harness.")
	persona := strings.Index(sent, "You are a coding agent powered by the fixture-model model.")
	plan := strings.Index(sent, strings.TrimSpace(officialPlanPolicy))
	read := strings.Index(sent, "Use the read tool")
	if identity < 0 || persona <= identity || plan <= persona || read <= plan {
		t.Fatalf("actual request did not contain ordered preset sections: %q", sent)
	}
	if !strings.Contains(sent, "catalog-fixture: A preserved catalog entry") || !strings.Contains(sent, filepath.Join(skillDir, "SKILL.md")) || strings.Contains(sent, "Load this body on demand.") {
		t.Fatalf("skill catalog was lost or eagerly loaded: %q", sent)
	}
	if strings.Contains(sent, "must explicitly approve via /plan off") {
		t.Fatal("obsolete plan approval policy reached the model")
	}
}

func TestSystemForMinimalDoesNotReceiveStandardPolicy(t *testing.T) {
	a, s := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		return core.Response{}, nil
	}))
	s.Preset = "minimal"
	got, err := a.systemFor(s)
	if err != nil || got != "You are a helpful software engineer assistant." {
		t.Fatalf("minimal system: %q, %v", got, err)
	}
}
