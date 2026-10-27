package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"dsgo/internal/core"
)

// Working-directory projection intentionally leaves Session.CWD untouched:
// it is the original project and remains the standing sandbox boundary.
// Source: packages/session/{working-directory,tool-working-directory}/src/index.ts.
var directoryOperations sync.Map // store root + session id -> *sync.Mutex

func workingDirectorySpec() core.ToolSpec {
	return toolSpec("working_directory", "Read the current working directory, or change it with cd. Relative paths use the current directory. Existing shells and running processes keep their own directories.", `{"type":"object","additionalProperties":false,"properties":{"cd":{"type":"string","description":"Existing directory to enter. Omit to read the current directory."}}}`)
}

func (a *App) registerWorkingDirectoryTool() error {
	return a.registry.Register(workingDirectorySpec(), a.workingDirectory)
}

func (a *App) activeWorkingDirectory(id string) (string, error) {
	session, err := a.store.Get(id)
	if err != nil {
		return "", err
	}
	directory := session.CWD
	if directory == "" {
		directory = a.flags.cwd
	}
	events, err := a.store.Events(id)
	if err != nil {
		return "", err
	}
	for _, event := range events {
		if event.Type != "working-directory/change" {
			continue
		}
		var change struct {
			CWD string `json:"cwd"`
		}
		if err := json.Unmarshal(event.Data, &change); err != nil || !filepath.IsAbs(change.CWD) {
			return "", errors.New("invalid persisted working-directory/change")
		}
		directory = change.CWD
	}
	if !filepath.IsAbs(directory) {
		return "", errors.New("working-directory: defaultDirectory must be absolute")
	}
	return directory, nil
}

// ensureWorkingDirectory validates the current value before request assembly.
// When a former directory disappeared, restoration is durable before returning.
func (a *App) ensureWorkingDirectory(ctx context.Context, id string) (string, error) {
	return a.changeWorkingDirectory(ctx, core.ToolContext{SessionID: id}, nil)
}

func (a *App) workingDirectory(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	if tc.SessionID == "" || a.store == nil {
		return core.ToolResult{}, errors.New("working_directory requires an agent Session")
	}
	var input struct {
		CD *string `json:"cd"`
	}
	if err := decodeOfficialObject(raw, &input); err != nil {
		return core.ToolResult{}, err
	}
	// JSON null is not an optional string in the official schema.
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	if value, ok := fields["cd"]; ok && string(value) == "null" {
		return core.ToolResult{}, errors.New("invalid tool input: cd must be a string")
	}
	directory, err := a.changeWorkingDirectory(ctx, tc, input.CD)
	return core.ToolResult{Content: directory}, err
}

func (a *App) changeWorkingDirectory(ctx context.Context, tc core.ToolContext, requested *string) (string, error) {
	if a.store == nil || tc.SessionID == "" {
		return "", errors.New("working_directory requires an agent Session")
	}
	lock, _ := directoryOperations.LoadOrStore(a.store.Root()+"\x00"+tc.SessionID, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	previous, err := a.activeWorkingDirectory(tc.SessionID)
	if err != nil {
		return "", err
	}
	var directory string
	recovered := false
	if requested == nil {
		if info, statErr := os.Stat(previous); statErr == nil && info.IsDir() {
			return previous, nil
		}
		session, err := a.store.Get(tc.SessionID)
		if err != nil {
			return "", err
		}
		original := session.CWD
		if original == "" {
			original = a.flags.cwd
		}
		directory, err = resolveWorkingDirectory(original, original)
		if err != nil {
			return "", err
		}
		recovered = true
	} else {
		if *requested == "" {
			return "", errors.New("working-directory: cd must not be empty")
		}
		directory, err = resolveWorkingDirectory(*requested, previous)
		if err != nil {
			return "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if previous == directory {
		return directory, nil
	}
	change := map[string]any{"cwd": directory}
	if tc.Emit != nil {
		err = tc.Emit("working-directory/change", change)
	} else {
		_, err = a.store.Append(tc.SessionID, tc.TurnID, "working-directory/change", change)
	}
	if err != nil {
		return "", err
	}
	// The immutable change is sufficient for the next request's required context.
	// Inject through the ordinary step inbox so notices follow every tool result;
	// a cancelled or unavailable inbox cannot undo the committed transition.
	text := fmt.Sprintf("The working directory changed from %s to %s.", officialJSONString(previous), officialJSONString(directory))
	if recovered {
		text = fmt.Sprintf("The working directory %s is unavailable. The working directory is now %s.", officialJSONString(previous), officialJSONString(directory))
	}
	if tc.Inject != nil {
		notice := core.TextMessage("user", text)
		notice.Source = map[string]any{"kind": "working-directory"}
		if noticeErr := tc.Inject(notice); noticeErr != nil && a.out != nil {
			a.line("working-directory: committed %s, but could not queue its notice: %s\n", clean(directory), clean(noticeErr.Error()))
		}
	}
	return directory, nil
}

func resolveWorkingDirectory(path, current string) (string, error) {
	target := path
	if !filepath.IsAbs(target) {
		target = filepath.Join(current, target)
	}
	directory, err := filepath.EvalSymlinks(target)
	if err == nil {
		directory, err = filepath.Abs(directory)
	}
	if err == nil {
		var info os.FileInfo
		info, err = os.Stat(directory)
		if err == nil && !info.IsDir() {
			err = errors.New("not a directory")
		}
	}
	if err != nil {
		return "", fmt.Errorf("working-directory: directory does not exist: %s", officialJSONString(path))
	}
	return directory, nil
}

func workingDirectoryContext(directory string) string {
	return fmt.Sprintf("Current working directory: %s.", officialJSONString(directory))
}
