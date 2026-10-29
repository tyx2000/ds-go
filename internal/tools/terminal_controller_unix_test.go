//go:build darwin || linux

package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHostTerminalNativePTYRetainsStateAndScrubsHarnessCredentials(t *testing.T) {
	t.Setenv("DSGO_TERMINAL_TEST_TOKEN", "must-not-inherit")
	c, err := NewTerminalController(TerminalControllerOptions{Shell: &TerminalShell{Path: "/bin/sh", Name: "sh", Args: []string{"-i"}}, ShellCandidates: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	cwd := t.TempDir()
	_, err = c.Create(context.Background(), "native-owner", cwd, TerminalCreate{ID: "native", Cols: 100, Rows: 18})
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.Attach(context.Background(), "native-owner", "native")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Write(context.Background(), []byte("DSGO_TEST_VALUE=preserved; printf '__%s_%s__\\n' 'VALUE' \"$DSGO_TEST_VALUE\"\r")); err != nil {
		t.Fatal(err)
	}
	nextTerminalMatching(t, a, func(s TerminalSnapshot) bool { return strings.Contains(s.Screen, "__VALUE_preserved__") })
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := c.Attach(context.Background(), "native-owner", "native")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	nextTerminalMatching(t, b, func(s TerminalSnapshot) bool { return strings.Contains(s.Screen, "__VALUE_preserved__") })
	if err = b.Write(context.Background(), []byte("printf '__%s_%s_%s_%s__\\n' 'RESUME' \"$DSGO_TEST_VALUE\" \"$DSH_SESSION_ID\" \"${DSGO_TERMINAL_TEST_TOKEN-unset}\"\r")); err != nil {
		t.Fatal(err)
	}
	nextTerminalMatching(t, b, func(s TerminalSnapshot) bool {
		return strings.Contains(s.Screen, "__RESUME_preserved_native-owner_unset__")
	})
	if err = b.Resize(context.Background(), 70, 12); err != nil {
		t.Fatal(err)
	}
	if err = b.Write(context.Background(), []byte("printf '__SIZE__'; stty size\r")); err != nil {
		t.Fatal(err)
	}
	nextTerminalMatching(t, b, func(s TerminalSnapshot) bool { return strings.Contains(s.Screen, "__SIZE__12 70") })
	if info := c.List("native-owner"); len(info) != 1 || info[0].Cols != 70 || info[0].Rows != 12 {
		t.Fatalf("resize metadata %#v", info)
	}
}

func TestHostTerminalNativeCleanupIncludesBackgroundChildren(t *testing.T) {
	c, err := NewTerminalController(TerminalControllerOptions{Shell: &TerminalShell{Path: "/bin/sh", Name: "sh", Args: []string{"-i"}}, ShellCandidates: []string{}, DisposeGraceMS: 100})
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
	_, err = c.Create(context.Background(), "owner", t.TempDir(), TerminalCreate{ID: "native", Cols: 100, Rows: 12})
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.Attach(context.Background(), "owner", "native")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err = a.Write(context.Background(), []byte("sleep 30 & printf '__%s_%s__\\n' 'CHILD' \"$(jobs -p)\"\r")); err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`__CHILD_([0-9]+)__`)
	snapshot := nextTerminalMatching(t, a, func(s TerminalSnapshot) bool { return pattern.MatchString(s.Screen) })
	pid, _ := strconv.Atoi(pattern.FindStringSubmatch(snapshot.Screen)[1])
	rows, err := terminalProcessSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	identity, found := rows[pid]
	if !found {
		t.Fatal("background child not observable")
	}
	c.mu.Lock()
	terminal := c.owners["owner"].terminals["native"]
	c.mu.Unlock()
	if inspector, ok := terminal.process.(interface {
		InspectActivity(context.Context) (string, uint64)
	}); !ok {
		t.Fatal("native provider cannot inspect owned work")
	} else if state, _ := inspector.InspectActivity(context.Background()); state != "busy" {
		t.Fatalf("background work did not protect unattended terminal: %s", state)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err = c.CloseTerminal(ctx, "owner", "native"); err != nil {
		t.Fatal(err)
	}
	rows, err = terminalProcessSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if terminalIdentityAlive(identity, rows) {
		t.Fatalf("background pid %d escaped cleanup", pid)
	}
}

func TestHostTerminalShellActivityEvidenceIsPrivateAndInputFenced(t *testing.T) {
	a, args, env, err := prepareTerminalShellActivity(TerminalShell{Path: "/bin/zsh", Args: []string{"-i"}}, []string{"ZDOTDIR=/old/startup", "PUBLIC=yes"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.dispose()
	if len(args) != 1 || args[0] != "-i" {
		t.Fatalf("zsh args %v", args)
	}
	if !strings.Contains(strings.Join(env, "\n"), "ZDOTDIR="+a.directory) {
		t.Fatalf("startup redirect missing %v", env)
	}
	startup, err := os.ReadFile(filepath.Join(a.directory, ".zshenv"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(startup), "ZDOTDIR='/old/startup'") || !strings.Contains(string(startup), "add-zle-hook-widget line-finish") {
		t.Fatalf("startup contract %s", startup)
	}
	state := filepath.Join(a.directory, "state")
	if err = os.WriteFile(state, []byte("123:1:idle\n"), 0600); err != nil {
		t.Fatal(err)
	}
	status, revision := a.inspect(123)
	if status != "idle" {
		t.Fatal(status)
	}
	a.invalidate()
	status, _ = a.inspect(123)
	if status != "unknown" {
		t.Fatal("old prompt authorized idle after input")
	}
	if err = os.WriteFile(state, []byte("123:2:busy\n"), 0600); err != nil {
		t.Fatal(err)
	}
	status, after := a.inspect(123)
	if status != "busy" || after <= revision {
		t.Fatal(status, after)
	}
	if err = os.WriteFile(state, []byte("999:3:idle\n"), 0600); err != nil {
		t.Fatal(err)
	}
	status, _ = a.inspect(123)
	if status != "unknown" {
		t.Fatal("foreign process supplied idle evidence")
	}
	unsupported, custom, _, err := prepareTerminalShellActivity(TerminalShell{Path: "/bin/bash", Args: []string{"--noprofile", "-i"}}, nil)
	if err != nil || unsupported != nil || fmt.Sprint(custom) != "[--noprofile -i]" {
		t.Fatal("custom launch was modified")
	}
}
