//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dsgo/internal/core"
)

func TestManagedFreshBashUsesCallingSessionAndWorkdir(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DSH_STALE_PARENT", "stale")
	t.Setenv("dsh_shell", "spoof")
	t.Setenv("DSH_SESSION_ID", "parent")
	t.Setenv("DSH_HOME", filepath.Join(root, "parent-home"))
	t.Setenv("VISIBLE_ORDINARY", "preserved")
	t.Setenv("THIRD_PARTY_PASSWORD", "not-inherited")
	r := New()
	defer r.Close()
	tc := core.ToolContext{SessionID: "owner-a", CWD: root, WorkspaceRoot: root, HarnessHome: filepath.Join(root, "data-a"), Permission: "full-access"}
	run := func(workdir, command string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		args := map[string]any{"description": "Inspect current managed shell facts", "command": command}
		if workdir != "" {
			args["workdir"] = workdir
		}
		result, err := r.Execute(ctx, tc, "standard_bash", core.JSON(args))
		if err != nil {
			t.Fatal(err)
		}
		var value struct {
			Stdout struct{ Text string }
			CWD    string
			Kind   string
		}
		if err := json.Unmarshal([]byte(result.Content), &value); err != nil {
			t.Fatal(err)
		}
		if value.Kind != "foreground" {
			t.Fatalf("unexpected result %s", result.Content)
		}
		return value.Stdout.Text
	}
	command := `printf '%s\n' "$DSH_HOME" "$DSH_SHELL" "$DSH_SESSION_ID" "${DSH_STALE_PARENT-unset}" "${DSH_PROFILE-unset}" "$VISIBLE_ORDINARY" "${THIRD_PARTY_PASSWORD-unset}" "$PWD"`
	canonical, _ := filepath.EvalSymlinks(root)
	want := tc.HarnessHome + "\n1\nowner-a\nunset\nunset\npreserved\nunset\n" + canonical + "\n"
	if got := run("", command); got != want {
		t.Fatalf("fresh facts: %q want %q", got, want)
	}
	if got := run("nested", `printf '%s|%s' "$PWD" "$DSH_SESSION_ID"`); got != filepath.Join(canonical, "nested")+"|owner-a" {
		t.Fatalf("workdir/session mismatch: %q", got)
	}
	if tc.CWD != root {
		t.Fatal("workdir override changed the calling session")
	}
	tc.SessionID = "owner-b"
	tc.HarnessHome = filepath.Join(root, "data-b")
	if got := run("", `printf '%s|%s' "$DSH_HOME" "$DSH_SESSION_ID"`); got != tc.HarnessHome+"|owner-b" {
		t.Fatalf("facts bled across scopes: %q", got)
	}
	startup := filepath.Join(root, "startup.sh")
	if err := os.WriteFile(startup, []byte("export FROM_USER_BASH_ENV=present\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASH_ENV", startup)
	if got := run("", `printf '%s' "${FROM_USER_BASH_ENV-unset}"`); got != "present" {
		t.Fatalf("ordinary BASH_ENV discarded: %q", got)
	}
}

func TestManagedBackgroundShellKeepsStartSnapshot(t *testing.T) {
	r := New()
	defer r.Close()
	tc := core.ToolContext{SessionID: "background-owner", CWD: t.TempDir(), Permission: "full-access", HarnessHome: filepath.Join(t.TempDir(), "state")}
	j, err := r.startStandardBash(tc, standardBashArgs{Description: "Inspect detached owner facts", Command: `sleep 0.05; printf '%s|%s' "$DSH_SESSION_ID" "$DSH_HOME"`}, "full-access", false)
	if err != nil {
		t.Fatal(err)
	}
	want := tc.SessionID + "|" + tc.HarnessHome
	tc.SessionID = "later-owner"
	tc.HarnessHome = "later-home"
	select {
	case <-j.done:
	case <-time.After(5 * time.Second):
		t.Fatal("background shell did not settle")
	}
	got := j.standard.stdout.value()["text"]
	if got != want {
		t.Fatalf("background identity %q want %q", got, want)
	}
}

func TestManagedPersistentBashIdentityAndCWDStayPerOwner(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DSH_HOME", "stale")
	t.Setenv("DSH_STALE_PARENT", "stale")
	r := New()
	defer r.Close()
	tc := core.ToolContext{SessionID: "owner-a", CWD: root, WorkspaceRoot: root, Permission: "full-access", HarnessHome: "not-a-terminal-fact", ProfileName: "web", ProfileDir: "not-a-terminal-fact"}
	run := func(command string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result, err := r.Execute(ctx, tc, "bash", core.JSON(map[string]any{"command": command}))
		if err != nil {
			t.Fatal(err)
		}
		return result.Content
	}
	command := `printf '%s|%s|%s|%s|%s|%s' "$DSH_SHELL" "$DSH_SESSION_ID" "$DSH_PTY_SESSION_ID" "${DSH_HOME-unset}" "${DSH_PROFILE-unset}" "${DSH_STALE_PARENT-unset}"`
	if got := run(command); !strings.Contains(got, "1|owner-a|pty-1|unset|unset|unset") {
		t.Fatalf("persistent initial facts: %q", got)
	}
	run("export KEEP=one; cd nested")
	if got := run(`printf '%s|%s' "$KEEP" "${PWD##*/}"`); !strings.Contains(got, "one|nested") {
		t.Fatalf("persistent cwd/env lost: %q", got)
	}
	tc.SessionID = "owner-b"
	if got := run(command); !strings.Contains(got, "1|owner-b|pty-2|unset|unset|unset") {
		t.Fatalf("persistent cross-owner facts: %q", got)
	}
	if got := run(`printf '%s|%s' "${KEEP-unset}" "${PWD##*/}"`); strings.Contains(got, "one|nested") {
		t.Fatalf("cross-owner state leak: %q", got)
	}
	tc.SessionID = "owner-a"
	r.InvalidateSession(tc.SessionID)
	if got := run(command); !strings.Contains(got, "1|owner-a|pty-3|unset|unset|unset") {
		t.Fatalf("terminal id reused after reset: %q", got)
	}
}
