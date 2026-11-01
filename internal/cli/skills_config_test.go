package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	instructions "dsgo/internal/context"
	"dsgo/internal/core"
	"dsgo/internal/state"
)

func TestSkillCatalogTracksCurrentDirectory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a, s := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		return core.Response{}, nil
	}))
	a.session = s
	child := filepath.Join(s.CWD, "child")
	for _, item := range []struct{ dir, name string }{{s.CWD, "root-only"}, {child, "child-only"}} {
		dir := filepath.Join(item.dir, ".agents", "skills", item.name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+item.name+"\ndescription: Fixture\n---\nInstruction body\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	a.skills, err = instructions.Skills(s.CWD)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.workingDirectory(a.ctx, contractsContext(a, s), core.JSON(map[string]any{"cd": "child"})); err != nil {
		t.Fatal(err)
	}
	prompt, err := a.systemFor(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "child-only") {
		t.Error("new directory's child-only skill absent from actual system prompt")
	}
	result, err := a.Execute(a.ctx, contractsContext(a, s), "load_skill", core.JSON(map[string]any{"name": "child-only"}))
	if err != nil || !strings.Contains(result.Content, "Instruction body") {
		t.Fatalf("setup: model load failed: %v", err)
	}
	if _, err := a.loadSkill("child-only"); err != nil {
		t.Errorf("model can load child-only but /skills cannot: %v", err)
	}
}
func TestJSONConfigSetIsNDJSON(t *testing.T) {
	home := t.TempDir()
	var out, errout strings.Builder
	code := Main([]string{"--home", home, "--json", "config", "set", "max_steps", "200"}, strings.NewReader(""), &out, &errout)
	if code != 0 {
		t.Fatalf("setup exited %d: %s", code, errout.String())
	}
	saved, loadErr := state.LoadConfig(home)
	if loadErr != nil || saved.MaxSteps != 200 {
		t.Fatalf("configuration not saved: %v %+v", loadErr, saved)
	}
	if !json.Valid([]byte(strings.TrimSpace(out.String()))) {
		t.Fatalf("successful --json config set emitted ordinary text: %q", out.String())
	}
}

func TestSkillCatalogAndLoadsUseSameDirectoryOverrides(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a, s := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		return core.Response{}, nil
	}))
	a.session = s
	child := filepath.Join(s.CWD, "child")
	for _, fixture := range []struct{ dir, description string }{{s.CWD, "ROOT_DESCRIPTION"}, {child, "CHILD_DESCRIPTION"}} {
		dir := filepath.Join(fixture.dir, ".agents", "skills", "shared")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: shared\ndescription: "+fixture.description+"\n---\n"+fixture.description+"_BODY"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, fixture := range []struct{ dir, description string }{{child, "CHILD_DESCRIPTION"}, {s.CWD, "ROOT_DESCRIPTION"}} {
		if _, err := a.workingDirectory(a.ctx, contractsContext(a, s), core.JSON(map[string]any{"cd": fixture.dir})); err != nil {
			t.Fatal(err)
		}
		prompt, err := a.systemFor(s)
		if err != nil || !strings.Contains(prompt, "shared: "+fixture.description) {
			t.Fatalf("wrong catalog: %v %s", err, prompt)
		}
		user, err := a.loadSkill("shared")
		if err != nil || !strings.Contains(user, fixture.description+"_BODY") {
			t.Fatalf("wrong CLI skill: %v %s", err, user)
		}
		model, err := a.Execute(a.ctx, contractsContext(a, s), "load_skill", core.JSON(map[string]any{"name": "shared"}))
		if err != nil || model.Content != user {
			t.Fatalf("model/CLI skill differ: %v %s", err, model.Content)
		}
		var output strings.Builder
		a.out = &output
		if _, err := a.slash("/skills"); err != nil || !strings.Contains(output.String(), fixture.description) {
			t.Fatalf("wrong displayed catalog: %v %s", err, output.String())
		}
	}
	if _, err := a.workingDirectory(a.ctx, contractsContext(a, s), core.JSON(map[string]any{"cd": child})); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(child); err != nil {
		t.Fatal(err)
	}
	if prompt, err := a.systemFor(s); err != nil || !strings.Contains(prompt, "ROOT_DESCRIPTION") {
		t.Fatalf("missing directory not recovered: %v %s", err, prompt)
	}
}

func TestJSONConfigFailureDoesNotConfirmOrChangeSavedValue(t *testing.T) {
	home := t.TempDir()
	var output, errors strings.Builder
	if code := Main([]string{"--home", home, "--json", "config", "set", "max_steps", "200"}, strings.NewReader(""), &output, &errors); code != 0 {
		t.Fatal(errors.String())
	}
	output.Reset()
	errors.Reset()
	if code := Main([]string{"--home", home, "--json", "config", "set", "max_steps", "-1"}, strings.NewReader(""), &output, &errors); code == 0 {
		t.Fatal("invalid value accepted")
	}
	if output.Len() != 0 || errors.Len() == 0 {
		t.Fatalf("false confirmation: %q / %q", output.String(), errors.String())
	}
	saved, err := state.LoadConfig(home)
	if err != nil || saved.MaxSteps != 200 {
		t.Fatalf("failed save changed accepted value: %+v %v", saved, err)
	}
}
