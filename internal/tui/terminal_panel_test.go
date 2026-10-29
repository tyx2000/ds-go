package tui

import (
	"bytes"
	"context"
	"dsgo/internal/tools"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestTerminalPanelInputDecoderDetachesAndPreservesFragmentedKeys(t *testing.T) {
	var decoder terminalInputDecoder
	first := decoder.feed([]byte("abc\x1b[5;"), false)
	if len(first) != 1 || string(first[0].data) != "abc" {
		t.Fatalf("first %#v", first)
	}
	second := decoder.feed([]byte("2~\x1b[A\x03"), false)
	if len(second) != 2 || second[0].kind != "history-up" || !bytes.Equal(second[1].data, []byte("\x1b[A\x03")) {
		t.Fatalf("keyboard was not forwarded %#v", second)
	}
	third := decoder.feed([]byte("before\x1dafter"), false)
	if len(third) != 2 || string(third[0].data) != "before" || third[1].kind != "detach" {
		t.Fatalf("detach %#v", third)
	}
}

func TestTerminalPanelDefaultNewUsesAndRemembersAvailableShell(t *testing.T) {
	if _, err := exec.LookPath("/bin/bash"); err != nil {
		t.Skip("bash unavailable")
	}
	c, err := tools.NewTerminalController(tools.TerminalControllerOptions{Shell: &tools.TerminalShell{Path: "/bin/bash", Name: "configured bash", Args: []string{"--noprofile", "--norc", "-i"}}, ShellCandidates: []string{"/bin/sh"}, DisposeGraceMS: 100})
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
	preferred, remembered := "/bin/sh", ""
	p := &terminalPanel{o: TerminalPanelOptions{Controller: c, Owner: "owner", CWD: t.TempDir(), PreferredShell: func() string { return preferred }, RememberShell: func(path string) { remembered = path }}}
	id, err := p.create(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	rows := c.List("owner")
	if len(rows) != 1 || rows[0].ID != id || rows[0].Shell.Path != "/bin/sh" || remembered != "/bin/sh" {
		t.Fatalf("default new ignored preference: %#v, remembered %q", rows, remembered)
	}
	if err = c.CloseTerminal(context.Background(), "owner", id); err != nil {
		t.Fatal(err)
	}
	preferred = "/missing/old-shell"
	remembered = ""
	id, err = p.create(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	rows = c.List("owner")
	if len(rows) != 1 || rows[0].ID != id || rows[0].Shell.Path != "/bin/bash" || remembered != "/bin/bash" {
		t.Fatalf("stale preference did not fall back to verified default: %#v, remembered %q", rows, remembered)
	}
}
func TestTerminalPanelBracketedPasteDoesNotTriggerLocalControls(t *testing.T) {
	var decoder terminalInputDecoder
	var output []byte
	data := []byte("\x1b[200~pasted\x1d\x1b[5;2~\x1b[201~")
	for _, b := range data {
		for _, command := range decoder.feed([]byte{b}, false) {
			if command.kind != "write" {
				t.Fatalf("paste invoked local command %s", command.kind)
			}
			output = append(output, command.data...)
		}
	}
	if !bytes.Equal(output, data) {
		t.Fatalf("paste changed %q to %q", data, output)
	}
	commands := decoder.feed([]byte{0x1d}, false)
	if len(commands) != 1 || commands[0].kind != "detach" {
		t.Fatalf("paste state leaked %#v", commands)
	}
}
func TestTerminalPanelInputModesAreExplicitlyLimited(t *testing.T) {
	for _, mode := range []int{1, 66, 1000, 1006, 2004} {
		if !terminalPanelInputMode(mode) {
			t.Fatal("supported input mode", mode)
		}
	}
	for _, mode := range []int{25, 47, 1049, 2026, -1} {
		if terminalPanelInputMode(mode) {
			t.Fatal("screen mode escaped input bridge", mode)
		}
	}
	if strings.Contains(terminalResetInputModes, "1049") {
		t.Fatal("reset input modes must not leave alternate screen")
	}
}
