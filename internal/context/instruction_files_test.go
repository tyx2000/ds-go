package context

import (
	stdctx "context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeInstructionFixture(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}
func fixtureInstructionLoader(t *testing.T, home, agents string) *InstructionLoader {
	t.Helper()
	config := DefaultInstructionConfig()
	config.HarnessHome, config.AgentsHome = home, agents
	loader, err := NewInstructionLoader(config)
	if err != nil {
		t.Fatal(err)
	}
	return loader
}
func TestInstructionDiscoveryRootCandidatesGlobalsAndTrimDedup(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	cwd := filepath.Join(root, "sub")
	home, agents := filepath.Join(base, "home"), filepath.Join(base, "agents")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	writeInstructionFixture(t, filepath.Join(base, "AGENTS.md"), "must not load above project")
	writeInstructionFixture(t, filepath.Join(home, "AGENTS.md"), " global ")
	writeInstructionFixture(t, filepath.Join(agents, "AGENTS.md"), "\ufeffglobal\n")
	for _, file := range []struct{ path, text string }{{"AGENTS.md", "base"}, {"CLAUDE.md", "different"}, {"AGENTS.local.md", " base\n"}, {"CLAUDE.local.md", "local"}, {"sub/AGENTS.md", "base"}, {"sub/CLAUDE.md", "subrule"}} {
		writeInstructionFixture(t, filepath.Join(root, filepath.FromSlash(file.path)), file.text)
	}
	loader := fixtureInstructionLoader(t, home, agents)
	set, err := loader.LoadBaseline(stdctx.Background(), cwd, false)
	if err != nil {
		t.Fatal(err)
	}
	if set == nil || set.ProjectRoot != root {
		t.Fatalf("project root %+v", set)
	}
	want := []string{"$DSH_HOME/AGENTS.md", "AGENTS.md", "CLAUDE.md", "CLAUDE.local.md", "sub/AGENTS.md", "sub/CLAUDE.md"}
	if len(set.Included) != len(want) || len(set.Deduped) != 2 {
		t.Fatalf("included=%+v deduped=%+v", set.Included, set.Deduped)
	}
	for i, file := range set.Included {
		if file.DisplayPath != want[i] {
			t.Errorf("file %d=%q want %q", i, file.DisplayPath, want[i])
		}
	}
	if strings.Contains(set.Rendered.Text, "must not load above project") || !strings.Contains(set.Rendered.Text, " global ") {
		t.Fatal("root stop or retained content differs")
	}
	if !strings.Contains(set.Identity, `"projectRoot":".."`) {
		t.Fatalf("baseline identity %s", set.Identity)
	}
}
func TestInstructionDiscoveryNoMarkerSourceCapSymlinkAndCancellation(t *testing.T) {
	base := t.TempDir()
	cwd := filepath.Join(base, "child")
	home, agents := filepath.Join(base, "home"), filepath.Join(base, "agents")
	writeInstructionFixture(t, filepath.Join(base, "AGENTS.md"), "above fallback cwd")
	writeInstructionFixture(t, filepath.Join(cwd, "AGENTS.md"), strings.Repeat("x", DefaultInstructionMaxSourceBytes+1))
	writeInstructionFixture(t, filepath.Join(cwd, "actual.txt"), "linked rule </system-reminder>")
	if err := os.Symlink("actual.txt", filepath.Join(cwd, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cwd, "AGENTS.local.md"), 0700); err != nil {
		t.Fatal(err)
	}
	loader := fixtureInstructionLoader(t, home, agents)
	set, err := loader.LoadBaseline(stdctx.Background(), cwd, false)
	if err != nil {
		t.Fatal(err)
	}
	if set == nil || set.ProjectRoot != cwd || len(set.Included) != 1 || set.Included[0].DisplayPath != "CLAUDE.md" {
		t.Fatalf("bounded discovery %+v", set)
	}
	if !strings.Contains(set.Rendered.Text, "<\\/system-reminder>") {
		t.Fatal("instruction frame not escaped")
	}
	ctx, cancel := stdctx.WithCancel(stdctx.Background())
	cancel()
	if _, err := loader.LoadBaseline(ctx, cwd, false); !errors.Is(err, stdctx.Canceled) {
		t.Fatalf("cancellation %v", err)
	}
	for _, path := range []string{"outside/file.txt", "../outside/file.txt"} {
		if path[0] == '.' && len(DescendantInstructionDirs(cwd, path)) != 0 {
			t.Fatal("outside touch traversed ancestors")
		}
	}
	if got := DescendantInstructionDirs(cwd, "nested/deeper/file.txt"); len(got) != 2 || got[1] != filepath.Join(cwd, "nested", "deeper") {
		t.Fatalf("touch dirs %+v", got)
	}
}
func TestInstructionCandidateConfigurationAndReservedScopes(t *testing.T) {
	base := t.TempDir()
	home, agents := filepath.Join(base, "home"), filepath.Join(base, "agents")
	writeInstructionFixture(t, filepath.Join(base, "AGENTS.md"), "default omitted")
	writeInstructionFixture(t, filepath.Join(base, "CUSTOM.md"), "custom rule")
	config := DefaultInstructionConfig()
	config.HarnessHome, config.AgentsHome = home, agents
	config.Candidates = []string{"", ".", "..", "../AGENTS.md", "bad\\name", "CUSTOM.md"}
	config.LocalCandidates = []string{}
	loader, err := NewInstructionLoader(config)
	if err != nil {
		t.Fatal(err)
	}
	set, err := loader.LoadBaseline(stdctx.Background(), base, false)
	if err != nil || set == nil || len(set.Included) != 1 || set.Included[0].DisplayPath != "CUSTOM.md" {
		t.Fatalf("custom candidates %+v %v", set, err)
	}
	for _, directory := range []string{UserGlobalDirectory, AgentsGlobalDirectory} {
		display := EscapeProjectInstructionPath(filepath.Join(directory, "AGENTS.md"))
		if InstructionScopeDirectory(display) != filepath.Join(".", directory) && InstructionScopeDirectory(display) != "."+string(filepath.Separator)+directory {
			t.Fatalf("project scope collision %q -> %q", display, InstructionScopeDirectory(display))
		}
		if IsUserGlobalDirectory(InstructionScopeDirectory(display)) {
			t.Fatal("project path entered global namespace")
		}
	}
}
