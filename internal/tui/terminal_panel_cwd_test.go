package tui

import (
	"context"
	"errors"
	"os"
	"runtime"
	"testing"
	"time"

	"dsgo/internal/tools"
)

func TestSCOR018TerminalPanelResolvesDirectoryAtEachCreate(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("native Host terminal provider unavailable")
	}
	c, err := tools.NewTerminalController(tools.TerminalControllerOptions{Shell: &tools.TerminalShell{Path: "/bin/sh", Name: "sh", Args: []string{"-i"}}, ShellCandidates: []string{}, DisposeGraceMS: 100})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := c.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	initial, latest := t.TempDir(), t.TempDir()
	directory, resolveErr := initial, error(nil)
	resolved := 0
	p := &terminalPanel{o: TerminalPanelOptions{Controller: c, Owner: "owner", CWD: initial, ResolveCWD: func(context.Context) (string, error) {
		resolved++
		return directory, resolveErr
	}}}
	if _, err := p.create(context.Background(), "/bin/sh"); err != nil {
		t.Fatal(err)
	}
	// The pane stays open while another actor changes this Session's directory.
	directory = latest
	if _, err := p.create(context.Background(), "/bin/sh"); err != nil {
		t.Fatal(err)
	}
	rows := c.List("owner")
	if len(rows) != 2 || resolved != 2 {
		t.Fatalf("pane reused its opening directory: %#v, lookups %d", rows, resolved)
	}
	for i, expected := range []string{initial, latest} {
		actualDirectory, err := os.Stat(rows[i].CWD)
		if err != nil {
			t.Fatal(err)
		}
		expectedDirectory, err := os.Stat(expected)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(actualDirectory, expectedDirectory) {
			t.Fatalf("terminal %d started in %q, expected directory %q", i, rows[i].CWD, expected)
		}
	}
	resolveErr = errors.New("directory cannot be restored")
	if _, err := p.create(context.Background(), "/bin/sh"); !errors.Is(err, resolveErr) {
		t.Fatalf("new shell ignored current-directory validation: %v", err)
	}
	if len(c.List("owner")) != 2 {
		t.Fatal("failed directory validation allocated a terminal")
	}
}
