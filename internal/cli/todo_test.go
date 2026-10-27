package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"dsgo/internal/core"
	"dsgo/internal/state"
	"dsgo/internal/tools"
)

func contractsApp(t *testing.T) (*App, core.Session) {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(t.TempDir(), "official contracts")
	if err != nil {
		t.Fatal(err)
	}
	a := &App{store: store, session: session, registry: tools.New(), out: io.Discard, err: io.Discard}
	t.Cleanup(func() { _ = a.registry.Close() })
	return a, session
}

func contractsContext(a *App, session core.Session) core.ToolContext {
	return core.ToolContext{SessionID: session.ID, CWD: session.CWD, TurnID: "fixture-turn", Emit: func(kind string, data any) error {
		_, err := a.store.Append(session.ID, "fixture-turn", kind, data)
		return err
	}}
}

func TestTodoOfficialWholeListAndTurnProjection(t *testing.T) {
	a, session := contractsApp(t)
	if err := a.registerTodoTool(); err != nil {
		t.Fatal(err)
	}
	tc := contractsContext(a, session)
	result, err := a.registry.Execute(context.Background(), tc, "todo_write", json.RawMessage(`{"todos":[{"content":"  first  ","status":"in_progress"},{"content":"\ufeffsecond\ufeff","status":"in_progress"},{"content":"third","status":"pending"}]}`))
	if err != nil || result.Content != "Updated todo list: 1 pending, 2 in progress, 0 completed." {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	todos, err := a.todoState(session.ID)
	if err != nil || len(todos) != 3 || todos[0].Content != "first" || todos[1].Content != "second" {
		t.Fatalf("todos=%#v err=%v", todos, err)
	}
	_, _ = a.store.Append(session.ID, "fixture-turn", "turn/finished", nil)
	if todos, err = a.todoState(session.ID); err != nil || len(todos) != 3 {
		t.Fatalf("finished checklist lost: %#v %v", todos, err)
	}
	_, _ = a.store.Append(session.ID, "next", "turn/started", nil)
	if todos, err = a.todoState(session.ID); err != nil || todos != nil {
		t.Fatalf("new turn retained old plan: %#v %v", todos, err)
	}
	_, err = a.registry.Execute(context.Background(), tc, "todo_write", json.RawMessage(`{"todos":[]}`))
	todos, readErr := a.todoState(session.ID)
	if err != nil || readErr != nil || todos == nil || len(todos) != 0 {
		t.Fatalf("empty snapshot not preserved: %#v %v %v", todos, err, readErr)
	}
	if got := renderTodos([]todoItem{{Content: "next", Status: "pending"}, {Content: "doing", Status: "in_progress"}, {Content: "done", Status: "completed"}}); got != "[ ] next\n[>] doing\n[x] done\n" {
		t.Fatalf("render=%q", got)
	}
}

func TestTodoRejectsInvalidInputAndFailedCommit(t *testing.T) {
	a, session := contractsApp(t)
	if err := a.registerTodoTool(); err != nil {
		t.Fatal(err)
	}
	tc := contractsContext(a, session)
	invalid := []string{`{}`, `{"todos":null}`, `{"todos":[{"content":" ","status":"pending"}]}`, `{"todos":[{"content":"a","status":"pending"},{"content":" a ","status":"completed"}]}`, `{"todos":[{"content":"a","status":"other"}]}`, `{"todos":[{"content":"a","status":"pending","id":"extra"}]}`, `{"todos":[],"scope":"shared"}`, `{"todos":[{"status":"pending"}]}`, `{"todos":[null]}`}
	for _, raw := range invalid {
		if _, err := a.registry.Execute(context.Background(), tc, "todo_write", json.RawMessage(raw)); err == nil {
			t.Errorf("accepted invalid input: %s", raw)
		}
	}
	events, _ := a.store.Events(session.ID)
	for _, event := range events {
		if event.Type == "todo/write" {
			t.Fatal("invalid input was persisted")
		}
	}
	if _, err := a.writeTodo(context.Background(), core.ToolContext{}, json.RawMessage(`{"todos":[]}`)); err == nil || !strings.Contains(err.Error(), "owning agent session") {
		t.Fatalf("accepted non-agent owner: %v", err)
	}
	tc.Emit = func(string, any) error { return errors.New("disk failure") }
	if _, err := a.writeTodo(context.Background(), tc, json.RawMessage(`{"todos":[]}`)); err == nil || err.Error() != "disk failure" {
		t.Fatalf("failed write claimed success: %v", err)
	}
}
