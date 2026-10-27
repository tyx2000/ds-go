package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"dsgo/internal/core"
)

// The fixed official todo contract is a whole-list, per-session snapshot.
// Source: packages/todo/tool-todo/src/index.ts at core.UpstreamCommit.
type todoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

type todoSnapshot struct {
	Todos []todoItem `json:"todos"`
}

func todoSpec() core.ToolSpec {
	return toolSpec("todo_write", "Record and update a task list to plan multi-step work and show progress; skip it for trivial single-step tasks. Add one todo per concrete step before you start. While work remains, keep the todos being worked on `in_progress`, several only when work runs in parallel. Mark each todo `completed` as soon as it is done.", `{"type":"object","additionalProperties":false,"properties":{"todos":{"type":"array","description":"The COMPLETE task list, replacing any previous list.","items":{"type":"object","additionalProperties":false,"properties":{"content":{"type":"string","description":"What the task is — a short imperative line."},"status":{"type":"string","enum":["pending","in_progress","completed"],"description":"pending (not started) | in_progress (now) | completed (done)."}},"required":["content","status"]}}},"required":["todos"]}`)
}

func (a *App) registerTodoTool() error { return a.registry.Register(todoSpec(), a.writeTodo) }

func (a *App) writeTodo(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	var input struct {
		Todos *[]todoItem `json:"todos"`
	}
	if err := decodeOfficialObject(raw, &input); err != nil {
		return core.ToolResult{}, err
	}
	if input.Todos == nil {
		return core.ToolResult{}, errors.New("invalid todos: `todos` must be an array")
	}
	// All official Web presets that mount this tool explicitly allow parallel
	// tasks, so there is deliberately no single-active restriction here.
	todos := make([]todoItem, 0, len(*input.Todos))
	seen := map[string]bool{}
	counts := map[string]int{"pending": 0, "in_progress": 0, "completed": 0}
	for _, item := range *input.Todos {
		item.Content = strings.TrimFunc(item.Content, jsWhitespace)
		if item.Content == "" {
			return core.ToolResult{}, errors.New("invalid todo: `content` must be a non-empty string")
		}
		if seen[item.Content] {
			return core.ToolResult{}, fmt.Errorf("invalid todos: duplicate content %s", officialJSONString(item.Content))
		}
		seen[item.Content] = true
		if _, ok := counts[item.Status]; !ok {
			return core.ToolResult{}, fmt.Errorf("invalid todo status %q", item.Status)
		}
		counts[item.Status]++
		todos = append(todos, item)
	}
	if tc.SessionID == "" || tc.Emit == nil || a.store == nil {
		return core.ToolResult{}, errors.New("todo_write requires an owning agent session")
	}
	if _, err := a.store.Get(tc.SessionID); err != nil {
		return core.ToolResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return core.ToolResult{}, err
	}
	if err := tc.Emit("todo/write", todoSnapshot{Todos: todos}); err != nil {
		return core.ToolResult{}, err
	}
	return core.ToolResult{Content: fmt.Sprintf("Updated todo list: %d pending, %d in progress, %d completed.", counts["pending"], counts["in_progress"], counts["completed"])}, nil
}

func (a *App) todoState(id string) ([]todoItem, error) {
	events, err := a.store.Events(id)
	if err != nil {
		return nil, err
	}
	var todos []todoItem
	for _, event := range events {
		switch event.Type {
		case "turn/start", "turn/started":
			todos = nil
		case "todo/write":
			var snapshot todoSnapshot
			if err := json.Unmarshal(event.Data, &snapshot); err != nil {
				return nil, fmt.Errorf("invalid persisted todo/write: %w", err)
			}
			todos = snapshot.Todos
		}
	}
	return todos, nil
}

func renderTodos(todos []todoItem) string {
	if todos == nil {
		return "No active todo list.\n"
	}
	if len(todos) == 0 {
		return "Todo list is empty.\n"
	}
	var out strings.Builder
	for _, item := range todos {
		mark := " "
		if item.Status == "in_progress" {
			mark = ">"
		} else if item.Status == "completed" {
			mark = "x"
		}
		fmt.Fprintf(&out, "[%s] %s\n", mark, clean(item.Content))
	}
	return out.String()
}

func (a *App) todoCommand(arg string) error {
	if strings.TrimSpace(arg) != "" {
		return errors.New("usage: /todo")
	}
	todos, err := a.todoState(a.session.ID)
	if err != nil {
		return err
	}
	if a.flags.json {
		a.print(todoSnapshot{Todos: todos})
	} else {
		a.line("%s", renderTodos(todos))
	}
	return nil
}

// ECMAScript String.trim whitespace, including BOM and excluding U+0085.
func jsWhitespace(r rune) bool {
	return r >= '\u2000' && r <= '\u200a' || strings.ContainsRune("\t\n\v\f\r \u00a0\u1680\u2028\u2029\u202f\u205f\u3000\ufeff", r)
}

func decodeOfficialObject(raw json.RawMessage, value any) error {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return errors.New("invalid tool input: expected an object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("invalid tool input: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("invalid tool input: expected one JSON object")
	}
	return nil
}

func officialJSONString(value string) string {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	return strings.NewReplacer(`\u2028`, "\u2028", `\u2029`, "\u2029").Replace(strings.TrimSuffix(out.String(), "\n"))
}
