package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"dsgo/internal/tools"
)

func TestHostTerminalShellPreferencePersistsAndStorageIsBestEffort(t *testing.T) {
	root := t.TempDir()
	a := &App{flags: flags{home: root}}
	a.rememberTerminalShell("/a shell/path")
	b := &App{flags: flags{home: root}}
	if got := b.preferredTerminalShell(); got != "/a shell/path" {
		t.Fatalf("preference %q", got)
	}
	info, err := os.Stat(filepath.Join(root, "terminal-preferences.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("preference mode %v", info.Mode())
	}
	var writers sync.WaitGroup
	for i := 0; i < 12; i++ {
		writers.Add(1)
		go func() { defer writers.Done(); a.rememberTerminalShell("/concurrent/shell") }()
	}
	writers.Wait()
	if got := b.preferredTerminalShell(); got != "/concurrent/shell" {
		t.Fatalf("atomic preference %q", got)
	}
	if err = os.WriteFile(filepath.Join(root, "terminal-preferences.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if b.preferredTerminalShell() != "" {
		t.Fatal("corrupt preference blocked fallback")
	}
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err = os.WriteFile(blocked, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	unavailable := &App{flags: flags{home: blocked}}
	unavailable.rememberTerminalShell("/bin/sh")
	if unavailable.preferredTerminalShell() != "" {
		t.Fatal("unavailable storage returned a selection")
	}
}

func TestHostTerminalShellPreferenceUsesOnlyVerifiedAvailableChoices(t *testing.T) {
	a := &App{flags: flags{home: t.TempDir()}}
	c, err := tools.NewTerminalController(tools.TerminalControllerOptions{Shell: &tools.TerminalShell{Path: "/bin/sh", Name: "sh", Args: []string{"-i"}}, ShellCandidates: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	a.rememberTerminalShell("/stale/missing/shell")
	selected, err := a.chooseTerminalShell(context.Background(), c, "")
	if err != nil || selected != "/bin/sh" {
		t.Fatal("missing preference did not choose verified default", selected, err)
	}
	if _, err = a.chooseTerminalShell(context.Background(), c, "/unverified/shell"); err == nil {
		t.Fatal("explicit unavailable shell silently fell back")
	}
	if len(c.List("owner")) != 0 {
		t.Fatal("shell discovery allocated a process")
	}
}

func TestHostTerminalCLIDefaultNewUsesRememberedShellAcrossClients(t *testing.T) {
	if _, err := exec.LookPath("/bin/bash"); err != nil {
		t.Skip("bash unavailable")
	}
	a, parent := lifecycleApp(t, lifecycleModel(nil))
	a.session = parent
	a.flags.json = true
	var out bytes.Buffer
	a.out = &out
	c, err := tools.NewTerminalController(tools.TerminalControllerOptions{Shell: &tools.TerminalShell{Path: "/bin/bash", Name: "configured bash", Args: []string{"--noprofile", "--norc", "-i"}}, ShellCandidates: []string{"/bin/sh"}, DisposeGraceMS: 100})
	if err != nil {
		t.Fatal(err)
	}
	a.hostTerminals = c
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := c.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if err = a.terminalCommand("new /bin/sh"); err != nil {
		t.Fatal(err)
	}
	var first tools.TerminalInfo
	if err = json.Unmarshal(out.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.Shell.Path != "/bin/sh" || a.preferredTerminalShell() != "/bin/sh" {
		t.Fatalf("explicit shell not remembered: %#v", first)
	}
	secondClient := &App{flags: flags{home: a.store.Root()}}
	if secondClient.preferredTerminalShell() != "/bin/sh" {
		t.Fatal("preference was scoped only to the live App")
	}
	if err = c.CloseTerminal(a.ctx, parent.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err = a.terminalCommand("new"); err != nil {
		t.Fatal(err)
	}
	var second tools.TerminalInfo
	if err = json.Unmarshal(out.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.Shell.Path != "/bin/sh" {
		t.Fatalf("default new ignored remembered available shell %#v", second)
	}
	a.rememberTerminalShell("/missing/previous-shell")
	out.Reset()
	if err = a.terminalCommand("list"); err != nil {
		t.Fatal(err)
	}
	if a.preferredTerminalShell() != "/missing/previous-shell" {
		t.Fatal("listing/recovering terminals changed shell preference")
	}
}
