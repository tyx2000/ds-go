package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"dsgo/internal/tools"
)

func TestHostTerminalCLIIsExplicitUserCapabilityAndNotModelTranscript(t *testing.T) {
	a, parent := lifecycleApp(t, lifecycleModel(nil))
	a.session = parent
	a.flags.json = true
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
	before, err := a.store.Events(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	specs := a.registry.Specs()
	if err = a.terminalCommand("new /bin/sh"); err != nil {
		t.Fatal(err)
	}
	var info tools.TerminalInfo
	if err = json.Unmarshal(out.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.ID == "" || info.CWD != parent.CWD {
		t.Fatalf("unexpected metadata %#v", info)
	}
	attachment, err := controller.Attach(a.ctx, parent.ID, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer attachment.Close()
	if err = attachment.Write(a.ctx, []byte("printf host-owned > user-terminal.txt\r")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	path := filepath.Join(parent.CWD, "user-terminal.txt")
	written := false
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(path)
		if string(data) == "host-owned" {
			written = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !written {
		t.Fatal("explicit user terminal inherited model's read-only sandbox")
	}
	a.registry.InvalidateSession(parent.ID)
	if len(controller.List(parent.ID)) != 1 {
		t.Fatal("model permission/runtime invalidation killed Host user terminal")
	}
	if err = a.terminalCommand("rename " + info.ID + " My terminal"); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err = a.terminalCommand("list"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "My terminal") {
		t.Fatalf("list %s", out.String())
	}
	if !reflect.DeepEqual(specs, a.registry.Specs()) {
		t.Fatal("Host terminal mutated model tool schemas")
	}
	after, err := a.store.Events(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatal("user terminal output was inserted into model event history")
	}
	other, err := a.store.Create(t.TempDir(), "other")
	if err != nil {
		t.Fatal(err)
	}
	a.session = other
	if err = a.terminalCommand("rename " + info.ID + " forbidden"); err == nil {
		t.Fatal("another Session renamed owned terminal")
	}
	if err = a.terminalCommand("attach " + info.ID); err == nil {
		t.Fatal("pipe-mode attach accepted without TTY")
	}
}
