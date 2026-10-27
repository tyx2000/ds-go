package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dsgo/internal/core"
)

func TestWorkingDirectoryPersistsWithoutMovingProjectBoundary(t *testing.T) {
	a, session := contractsApp(t)
	child := filepath.Join(session.CWD, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	if err := a.registerWorkingDirectoryTool(); err != nil {
		t.Fatal(err)
	}
	tc := contractsContext(a, session)
	var notices []core.Message
	tc.Inject = func(message core.Message) error { notices = append(notices, message); return nil }
	result, err := a.registry.Execute(context.Background(), tc, "working_directory", json.RawMessage(`{"cd":"child"}`))
	want, _ := filepath.EvalSymlinks(child)
	if err != nil || result.Content != want {
		t.Fatalf("result=%#v err=%v want=%s", result, err, want)
	}
	if len(notices) != 1 || notices[0].Role != "user" || notices[0].Source == nil || !strings.Contains(notices[0].Content[0].Text, "working directory changed from") {
		t.Fatalf("unattributed/missing transition notice: %#v", notices)
	}
	active, err := a.activeWorkingDirectory(session.ID)
	if err != nil || active != want {
		t.Fatalf("active=%s err=%v", active, err)
	}
	unchanged, _ := a.store.Get(session.ID)
	if unchanged.CWD != session.CWD {
		t.Fatal("working_directory expanded original project boundary")
	}
	fork, err := a.store.Fork(session.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if forkActive, err := a.activeWorkingDirectory(fork.ID); err != nil || forkActive != want {
		t.Fatalf("fork lost committed directory: %s %v", forkActive, err)
	}
	other, _ := a.store.Create(t.TempDir(), "other")
	if otherActive, err := a.activeWorkingDirectory(other.ID); err != nil || otherActive != other.CWD {
		t.Fatalf("directory leaked to another session: %s %v", otherActive, err)
	}
	result, err = a.registry.Execute(context.Background(), tc, "working_directory", json.RawMessage(`{}`))
	if err != nil || result.Content != want {
		t.Fatalf("read result=%#v err=%v", result, err)
	}
	if got := workingDirectoryContext(want); got != "Current working directory: "+string(core.JSON(want))+"." {
		t.Fatalf("context=%q", got)
	}
}

func TestWorkingDirectoryRecoversRemovedDirectoryAndRejectsInvalidChanges(t *testing.T) {
	a, session := contractsApp(t)
	tc := contractsContext(a, session)
	directory := filepath.Join(session.CWD, "gone")
	_ = os.Mkdir(directory, 0700)
	if _, err := a.workingDirectory(context.Background(), tc, core.JSON(map[string]any{"cd": "gone"})); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(directory); err != nil {
		t.Fatal(err)
	}
	got, err := a.ensureWorkingDirectory(context.Background(), session.ID)
	want, _ := filepath.EvalSymlinks(session.CWD)
	if err != nil || got != want {
		t.Fatalf("recovery=%s err=%v want=%s", got, err, want)
	}
	for _, raw := range []string{`{"cd":""}`, `{"cd":null}`, `{"cd":"missing"}`, `{"cd":42}`, `{"cwd":"."}`} {
		if _, err := a.workingDirectory(context.Background(), tc, json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	before, _ := a.store.Events(session.ID)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.workingDirectory(ctx, tc, core.JSON(map[string]any{"cd": t.TempDir()})); err == nil {
		t.Fatal("cancelled directory change committed")
	}
	after, _ := a.store.Events(session.ID)
	if len(before) != len(after) {
		t.Fatal("cancelled operation changed log")
	}
	if _, err := a.workingDirectory(context.Background(), core.ToolContext{}, json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "requires an agent Session") {
		t.Fatalf("non-agent request=%v", err)
	}
}
