package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"dsgo/internal/core"
	"dsgo/internal/tools"
)

func TestSCOR018HostTerminalCreateUsesCurrentWorkingDirectory(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("native Host terminal provider unavailable")
	}
	a, session := lifecycleApp(t, lifecycleModel(nil))
	a.session, a.flags.json = session, true
	var out bytes.Buffer
	a.out = &out
	controller, err := tools.NewTerminalController(tools.TerminalControllerOptions{Shell: &tools.TerminalShell{Path: "/bin/sh", Name: "sh", Args: []string{"-i"}}, ShellCandidates: []string{}, DisposeGraceMS: 100})
	if err != nil {
		t.Fatal(err)
	}
	a.hostTerminals = controller
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := controller.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	child := filepath.Join(session.CWD, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := a.workingDirectory(a.ctx, core.ToolContext{SessionID: session.ID}, core.JSON(map[string]any{"cd": "child"})); err != nil {
		t.Fatal(err)
	}
	if err := a.terminalCommand("new /bin/sh"); err != nil {
		t.Fatal(err)
	}
	var info tools.TerminalInfo
	if err := json.Unmarshal(out.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	actualDirectory, err := os.Stat(info.CWD)
	if err != nil {
		t.Fatal(err)
	}
	expectedDirectory, err := os.Stat(child)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(actualDirectory, expectedDirectory) {
		t.Fatalf("terminal started in %q, current directory is %q", info.CWD, child)
	}
	// Lost current directories affect new shells, not access to an already
	// retained terminal. Reading environment/list metadata must not restore it.
	if err := os.Remove(child); err != nil {
		t.Fatal(err)
	}
	before, err := a.store.Events(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := a.terminalCommand("list"); err != nil {
		t.Fatal(err)
	}
	after, err := a.store.Events(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatal("terminal metadata access restored the Session's missing directory")
	}
	attachment, err := controller.Attach(a.ctx, session.ID, info.ID)
	if err != nil {
		t.Fatal("retained terminal became unavailable with its former directory:", err)
	}
	attachment.Close()
}
