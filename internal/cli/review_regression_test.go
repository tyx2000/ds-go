package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"dsgo/internal/agent"
	"dsgo/internal/core"
	"dsgo/internal/state"
)

func TestReviewLateFlagsAndJSONHelp(t *testing.T) {
	home := t.TempDir()
	var out, errOut bytes.Buffer
	if code := Main([]string{"--home", home, "config", "show", "--json"}, strings.NewReader(""), &out, &errOut); code != 0 || !json.Valid(out.Bytes()) {
		t.Fatalf("config show --json: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Main([]string{"--home", home, "--json", "help"}, strings.NewReader(""), &out, &errOut); code != 0 || !json.Valid(out.Bytes()) {
		t.Fatalf("--json help: code=%d out=%q", code, out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Main([]string{"--home", home, "run", "task text", "--json"}, strings.NewReader(""), &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "flags must come before") {
		t.Fatalf("late run flag: code=%d err=%q", code, errOut.String())
	}
}

func TestReviewSelectSessionUsesCurrentWorkspace(t *testing.T) {
	a, parent := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		return core.Response{}, nil
	}))
	a.session = parent
	otherCWD := t.TempDir()
	other, err := a.store.Create(otherCWD, "other")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.selectSession(other.ID); err != nil {
		t.Fatal(err)
	}
	created, err := a.store.Create(otherCWD, "created")
	if err != nil {
		t.Fatal(err)
	}
	_ = a.store.Delete(created.ID)
	if err := a.selectSession(""); err != nil {
		t.Fatal(err)
	}
	if a.session.CWD != other.CWD {
		t.Fatalf("new session cwd=%q want %q", a.session.CWD, other.CWD)
	}
}

func TestReviewSlashCommandKeepsCurrentTitleAndPlan(t *testing.T) {
	a, parent := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		return core.Response{}, nil
	}))
	a.session = parent
	if _, err := a.store.Patch(parent.ID, func(s *core.Session) error {
		s.Title = "automatic title"
		s.Plan = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// a.session is the stale snapshot the slash command would previously write back.
	if _, err := a.slash("/model fixture-model-2"); err != nil {
		t.Fatal(err)
	}
	current, err := a.store.Get(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Title != "automatic title" || !current.Plan || current.Model != "fixture-model-2" {
		t.Fatalf("slash command clobbered session: %+v", current)
	}
	if err := a.selectSession(""); err != nil {
		t.Fatal(err)
	}
	if a.session.Model != "fixture-model-2" {
		t.Fatalf("new session did not inherit the selected model: %q", a.session.Model)
	}
}

func TestReviewSessionPatchPreservesPlanAndArchive(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(t.TempDir(), "patch")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Patch(session.ID, func(s *core.Session) error { s.Plan = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Patch(session.ID, func(s *core.Session) error { s.Title = "renamed"; return nil }); err != nil {
		t.Fatal(err)
	}
	current, _ := store.Get(session.ID)
	if !current.Plan || current.Title != "renamed" {
		t.Fatalf("patch lost fields: %+v", current)
	}
	_ = agent.Notice{}
}
