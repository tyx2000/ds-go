package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"dsgo/internal/tools"
	"dsgo/internal/tui"
)

// terminalCommand is an explicit Host-user action. The controller is never
// exposed in Registry, Agent tool schemas, PTC bindings or the model transcript.
func (a *App) terminalCommand(arg string) error {
	a.inputMu.Lock()
	defer a.inputMu.Unlock()
	owner := a.session.ID
	if owner == "" {
		return errors.New("select a Session before opening its terminals")
	}
	cwd := a.session.CWD
	if a.store != nil {
		current, err := a.activeWorkingDirectory(owner)
		if err != nil {
			return err
		}
		cwd = current
	}
	resolveCWD := func(ctx context.Context) (string, error) {
		if a.store == nil {
			return cwd, nil
		}
		return a.ensureWorkingDirectory(ctx, owner)
	}
	if a.hostTerminals == nil {
		controller, err := tools.NewTerminalController(userTerminalOptions(a.cfg.UserTerminal))
		if err != nil {
			return err
		}
		a.hostTerminals = controller
	}
	c := a.hostTerminals
	output, outputTTY := a.out.(*os.File)
	interactive := !a.flags.json && outputTTY && tui.IsTerminal(os.Stdin) && tui.IsTerminal(output)
	panel := tui.TerminalPanelOptions{Input: os.Stdin, Terminal: output, Output: trajectoryWriter{a}, Controller: c, Owner: owner, CWD: cwd, ResolveCWD: resolveCWD, Window: a.ctx, PreferredShell: a.preferredTerminalShell, RememberShell: a.rememberTerminalShell}
	arg = strings.TrimSpace(arg)
	action, rest, _ := strings.Cut(arg, " ")
	rest = strings.TrimSpace(rest)
	if action == "" {
		if interactive {
			return tui.TerminalPanel(a.ctx, panel)
		}
		a.print(c.List(owner))
		return nil
	}
	switch action {
	case "list":
		if rest != "" {
			return terminalCommandUsage()
		}
		a.print(c.List(owner))
		return nil
	case "shells":
		if rest != "" {
			return terminalCommandUsage()
		}
		shells, err := c.Shells(a.ctx)
		if err != nil {
			return err
		}
		a.print(shells)
		return nil
	case "new":
		path, err := a.chooseTerminalShell(a.ctx, c, rest)
		if err != nil {
			return err
		}
		a.rememberTerminalShell(path)
		cwd, err := resolveCWD(a.ctx)
		if err != nil {
			return err
		}
		limits := c.Environment(cwd)
		cols, rows := min(80, limits.MaxCols), min(24, limits.MaxRows)
		id := tools.NewHostTerminalID()
		info, err := c.Create(a.ctx, owner, cwd, tools.TerminalCreate{ID: id, ShellPath: path, Cols: cols, Rows: rows})
		if err != nil {
			return err
		}
		if _, err = c.Retain(a.ctx, owner, id); err != nil {
			return err
		}
		if interactive {
			panel.InitialID = id
			return tui.TerminalPanel(a.ctx, panel)
		}
		a.print(info)
		return nil
	case "attach":
		if rest == "" || strings.ContainsAny(rest, " \t\r\n") {
			return terminalCommandUsage()
		}
		if !interactive {
			return errors.New("terminal attach requires interactive standard input and output; use /terminal list to inspect metadata")
		}
		panel.InitialID = rest
		return tui.TerminalPanel(a.ctx, panel)
	case "rename":
		id, title, found := strings.Cut(rest, " ")
		if !found {
			return terminalCommandUsage()
		}
		return c.Rename(owner, id, strings.TrimSpace(title))
	case "close":
		if rest == "" || strings.ContainsAny(rest, " \t\r\n") {
			return terminalCommandUsage()
		}
		return c.CloseTerminal(a.ctx, owner, rest)
	default:
		return terminalCommandUsage()
	}
}
func terminalCommandUsage() error {
	return fmt.Errorf("usage: /terminal [list|shells|new [SHELL_PATH]|attach ID|rename ID TITLE|close ID]")
}
