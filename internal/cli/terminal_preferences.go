package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"dsgo/internal/tools"
)

// This is Client preference state, separate from the Host shell deployment
// profile. As with official browser storage, failure never prevents a launch.
type terminalPreferences struct {
	Shell string `json:"shell"`
}

func (a *App) terminalPreferenceRoot() string {
	if a.store != nil {
		return a.store.Root()
	}
	return a.flags.home
}
func (a *App) preferredTerminalShell() string {
	root := a.terminalPreferenceRoot()
	if root == "" {
		return ""
	}
	f, err := os.OpenFile(filepath.Join(root, "terminal-preferences.json"), os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 65536 {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(data) > 65536 {
		return ""
	}
	var preferences terminalPreferences
	if json.Unmarshal(data, &preferences) != nil || strings.IndexByte(preferences.Shell, 0) >= 0 {
		return ""
	}
	return preferences.Shell
}
func (a *App) rememberTerminalShell(path string) {
	root := a.terminalPreferenceRoot()
	if root == "" || len(path) > 60000 || strings.IndexByte(path, 0) >= 0 {
		return
	}
	data, err := json.Marshal(terminalPreferences{Shell: path})
	if err != nil {
		return
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return
	}
	f, err := os.CreateTemp(root, ".terminal-preferences-*")
	if err != nil {
		return
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(append(data, '\n')); err != nil {
		f.Close()
		return
	}
	if err = f.Close(); err != nil {
		return
	}
	_ = os.Rename(name, filepath.Join(root, "terminal-preferences.json"))
}

func (a *App) chooseTerminalShell(ctx context.Context, c *tools.TerminalController, requested string) (string, error) {
	shells, err := c.Shells(ctx)
	if err != nil {
		return "", err
	}
	path := requested
	if path == "" {
		path = a.preferredTerminalShell()
	}
	for _, shell := range shells {
		if shell.Path == path {
			return shell.Path, nil
		}
	}
	if requested != "" {
		return "", errors.New("selected shell is unavailable")
	}
	if len(shells) == 0 {
		return "", errors.New("no shell is available in this execution environment")
	}
	return shells[0].Path, nil
}
