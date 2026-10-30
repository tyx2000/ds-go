package tui

import (
	"context"
	"errors"
	"fmt"
	"github.com/charmbracelet/x/ansi"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"dsgo/internal/tools"
)

type TerminalPanelOptions struct {
	Input                 *os.File
	Terminal              *os.File
	Output                io.Writer
	Controller            *tools.TerminalController
	Owner, CWD, InitialID string
	Window                context.Context
	ResolveCWD            func(context.Context) (string, error) // Validate the Session's latest directory only when creating a terminal.
	PreferredShell        func() string
	RememberShell         func(string)
}
type terminalPanel struct {
	o                  TerminalPanelOptions
	selected           int
	shells             []tools.TerminalShell
	creating, renaming bool
	title, status      string
	preferred          string
	keys               pager
}

// TerminalPanel owns the foreground TTY only while open. Native processes and
// their retained screens belong to the Host controller, never to this pane.
func TerminalPanel(ctx context.Context, o TerminalPanelOptions) error {
	if o.Input == nil || o.Output == nil || o.Controller == nil || o.Owner == "" {
		return errors.New("terminal panel requires a Session controller and TTY")
	}
	restore, err := rawTerminal(o.Input)
	if err != nil {
		return err
	}
	defer restore()
	if _, err = io.WriteString(o.Output, "\x1b[?1049h\x1b[?25l"); err != nil {
		return err
	}
	defer func() { _, _ = io.WriteString(o.Output, terminalResetInputModes+"\x1b[0m\x1b[?25h\x1b[?1049l") }()
	p := &terminalPanel{o: o}
	if o.InitialID != "" {
		return p.attach(ctx, o.InitialID)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows := o.Controller.List(o.Owner)
		if p.creating {
			p.selected = min(p.selected, max(0, len(p.shells)-1))
		} else {
			p.selected = min(p.selected, max(0, len(rows)-1))
		}
		if err = p.draw(rows); err != nil {
			return err
		}
		var buffer [256]byte
		n, readErr := o.Input.Read(buffer[:])
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		if n == 0 && len(p.keys.key) == 1 && p.keys.key[0] == 0x1b {
			p.keys.key = nil
			p.creating = false
			p.renaming = false
			continue
		}
		if n == 0 {
			continue
		}
		if strings.IndexByte(string(buffer[:n]), 0x1d) >= 0 {
			return nil
		}
		for _, key := range p.keys.decodeKeys(buffer[:n]) {
			if p.renaming {
				switch key {
				case "escape":
					p.renaming = false
				case "enter":
					if len(rows) > 0 {
						err = o.Controller.Rename(o.Owner, rows[p.selected].ID, p.title)
						p.setStatus(err)
					}
					p.renaming = false
				case "backspace":
					r := []rune(p.title)
					if len(r) > 0 {
						p.title = string(r[:len(r)-1])
					}
				default:
					if utf8.RuneCountInString(key) == 1 && len(p.title) < 480 {
						p.title += Safe(key)
					}
				}
				continue
			}
			switch key {
			case "q", "ctrl-c":
				return nil
			case "escape":
				p.creating = false
			case "up", "k":
				p.selected = max(0, p.selected-1)
			case "down", "j":
				limit := len(rows)
				if p.creating {
					limit = len(p.shells)
				}
				p.selected = min(max(0, limit-1), p.selected+1)
			case "n":
				p.creating = false
				id, createErr := p.create(ctx, "")
				err = createErr
				if err == nil {
					err = p.attach(ctx, id)
				}
				p.setStatus(err)
			case "s":
				p.shells, err = o.Controller.Shells(ctx)
				p.setStatus(err)
				p.creating = err == nil
				p.selected = 0
				p.refreshPreference()
				for i, shell := range p.shells {
					if shell.Path == p.preferred {
						p.selected = i
						break
					}
				}
			case "enter":
				err = nil
				id := ""
				if p.creating && len(p.shells) > 0 {
					id, err = p.create(ctx, p.shells[p.selected].Path)
					p.creating = false
				} else if len(rows) > 0 {
					id = rows[p.selected].ID
				}
				if err == nil && id != "" {
					err = p.attach(ctx, id)
				}
				p.setStatus(err)
			case "r":
				if !p.creating && len(rows) > 0 {
					p.renaming = true
					p.title = rows[p.selected].Title
				}
			case "c":
				if !p.creating && len(rows) > 0 {
					err = o.Controller.CloseTerminal(ctx, o.Owner, rows[p.selected].ID)
					p.setStatus(err)
				}
			}
		}
	}
}
func (p *terminalPanel) setStatus(err error) {
	p.status = ""
	if err != nil {
		p.status = Safe(err.Error())
	}
}
func (p *terminalPanel) refreshPreference() {
	if p.o.PreferredShell != nil {
		p.preferred = p.o.PreferredShell()
	}
}
func (p *terminalPanel) create(ctx context.Context, path string) (string, error) {
	if path == "" {
		shells, err := p.o.Controller.Shells(ctx)
		if err != nil {
			return "", err
		}
		if len(shells) == 0 {
			return "", errors.New("no shell is available in this execution environment")
		}
		p.refreshPreference()
		path = shells[0].Path
		for _, shell := range shells {
			if shell.Path == p.preferred {
				path = shell.Path
				break
			}
		}
	}
	p.preferred = path
	if p.o.RememberShell != nil {
		p.o.RememberShell(path)
	}
	cols, rows := p.grid()
	id := tools.NewHostTerminalID()
	cwd := p.o.CWD
	if p.o.ResolveCWD != nil {
		var err error
		cwd, err = p.o.ResolveCWD(ctx)
		if err != nil {
			return "", err
		}
	}
	if _, err := p.o.Controller.Create(ctx, p.o.Owner, cwd, tools.TerminalCreate{ID: id, ShellPath: path, Cols: cols, Rows: rows}); err != nil {
		return "", err
	}
	window := p.o.Window
	if window == nil {
		window = ctx
	}
	if _, err := p.o.Controller.Retain(window, p.o.Owner, id); err != nil {
		return "", err
	}
	return id, nil
}
func (p *terminalPanel) grid() (int, int) {
	w, h := terminalSize(p.o.Terminal)
	limits := p.o.Controller.Environment(p.o.CWD)
	return max(2, min(w, limits.MaxCols)), max(1, min(h-2, limits.MaxRows))
}
func (p *terminalPanel) draw(rows []tools.TerminalInfo) error {
	w, h := terminalSize(p.o.Terminal)
	var lines []string
	title := "Session terminals"
	if p.creating {
		title = "New terminal · choose shell"
		for i, shell := range p.shells {
			mark := "  "
			if i == p.selected {
				mark = "> "
			}
			lines = append(lines, mark+Safe(shell.Name)+"  "+Safe(shell.Path))
		}
	} else {
		for i, row := range rows {
			mark := "  "
			if i == p.selected {
				mark = "> "
			}
			lines = append(lines, fmt.Sprintf("%s%s  [%s]  %s", mark, Safe(row.Title), row.State, row.ID))
		}
		if len(rows) == 0 {
			lines = []string{"No terminals. Press n to create one, or s to choose a shell."}
		}
	}
	help := "n new · s shell menu · Enter attach · r rename · c close · q return"
	if p.creating {
		help = "↑/↓ choose shell · Enter create · Esc cancel"
	}
	if p.renaming {
		help = "Rename: " + Safe(p.title) + "  (Enter save · Esc cancel)"
	}
	start := max(0, p.selected-(h-5))
	if start > len(lines) {
		start = 0
	}
	lines = lines[start:]
	var out strings.Builder
	out.WriteString(terminalResetInputModes + "\x1b[?25l\x1b[0m\x1b[2J\x1b[H")
	frameLine(&out, 1, clipCells(title, w))
	for i, line := range lines {
		if i >= h-4 {
			break
		}
		frameLine(&out, i+3, clipCells(line, w))
	}
	frameLine(&out, h-1, clipCells(help, w))
	frameLine(&out, h, clipCells(p.status, w))
	_, err := io.WriteString(p.o.Output, out.String())
	return err
}

const terminalResetInputModes = "\x1b[?1l\x1b[?9l\x1b[?66l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1004l\x1b[?1005l\x1b[?1006l\x1b[?1015l\x1b[?2004l"

func (p *terminalPanel) attach(ctx context.Context, id string) error {
	attachmentCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	a, err := p.o.Controller.Attach(attachmentCtx, p.o.Owner, id)
	if err != nil {
		return err
	}
	defer a.Close()
	cols, rows := p.grid()
	if err = a.Resize(ctx, cols, rows); err != nil {
		var refusal *tools.TerminalError
		if !errors.As(err, &refusal) || refusal.Code != "terminal/control-unavailable" {
			return err
		}
	}
	var current tools.TerminalSnapshot
	input := terminalInputDecoder{}
	history := 0
	dirty := true
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		newCols, newRows := p.grid()
		if newCols != cols || newRows != rows {
			cols, rows = newCols, newRows
			if current.Writable {
				if err = a.Resize(ctx, cols, rows); err != nil {
					p.setStatus(err)
				}
			}
			dirty = true
		}
		snapshotCtx, snapshotCancel := context.WithTimeout(ctx, time.Millisecond)
		next, nextErr := a.Next(snapshotCtx)
		snapshotCancel()
		if nextErr == nil {
			current = next
			dirty = true
		} else if !errors.Is(nextErr, context.DeadlineExceeded) {
			return nextErr
		}
		if dirty || current.Info.ID != "" {
			if err = p.drawScreen(a, current, history); err != nil {
				return err
			}
			dirty = false
		}
		var buffer [4096]byte
		n, readErr := p.o.Input.Read(buffer[:])
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		commands := input.feed(buffer[:n], n == 0)
		for _, command := range commands {
			switch command.kind {
			case "detach":
				return nil
			case "history-up":
				history += max(1, rows-1)
				dirty = true
			case "history-down":
				history = max(0, history-max(1, rows-1))
				dirty = true
			case "write":
				if !current.Writable {
					continue
				}
				history = 0
				if err = a.Write(ctx, command.data); err != nil {
					var refusal *tools.TerminalError
					if errors.As(err, &refusal) && refusal.Code == "terminal/control-unavailable" {
						current.Writable = false
						p.setStatus(err)
						dirty = true
						continue
					}
					return err
				}
			}
		}
	}
}
func (p *terminalPanel) drawScreen(a *tools.TerminalAttachment, s tools.TerminalSnapshot, history int) error {
	w, h := terminalSize(p.o.Terminal)
	var out strings.Builder
	out.WriteString("\x1b[?25l\x1b[0m\x1b[2J")
	lines := strings.Split(s.Screen, "\n")
	if history > 0 {
		var err error
		lines, err = a.History(history-1, max(1, h-2))
		if err != nil {
			return err
		}
	}
	for i, line := range lines {
		if i >= h-2 {
			break
		}
		frameLine(&out, i+1, ansi.Truncate(line, w, ""))
	}
	state := s.Info.State
	if s.Info.ExitCode != nil {
		state = fmt.Sprintf("%s %d", state, *s.Info.ExitCode)
	}
	control := "read-only"
	if s.Writable {
		control = "input enabled"
	}
	footer := fmt.Sprintf("%s · %s · %s · Ctrl+] return · Shift+PgUp/PgDn history", Safe(s.Info.Title), state, control)
	frameLine(&out, h-1, clipCells(footer, w))
	frameLine(&out, h, clipCells(p.status, w))
	out.WriteString(terminalResetInputModes)
	if history == 0 && s.Writable {
		for _, mode := range s.InputModes {
			if terminalPanelInputMode(mode) {
				fmt.Fprintf(&out, "\x1b[?%dh", mode)
			}
		}
	}
	if history == 0 && s.CursorVisible {
		fmt.Fprintf(&out, "\x1b[%d;%dH\x1b[?25h", min(h-2, max(1, s.CursorRow+1)), min(w, max(1, s.CursorCol+1)))
	}
	_, err := io.WriteString(p.o.Output, out.String())
	return err
}
func terminalPanelInputMode(mode int) bool {
	switch mode {
	case 1, 9, 66, 1000, 1002, 1003, 1004, 1005, 1006, 1015, 2004:
		return true
	}
	return false
}
func frameLine(out *strings.Builder, row int, line string) {
	fmt.Fprintf(out, "\x1b[%d;1H\x1b[0m\x1b[2K%s", row, line)
}

type terminalInputCommand struct {
	kind string
	data []byte
}
type terminalInputDecoder struct {
	pending []byte
	pasting bool
}

func (d *terminalInputDecoder) feed(data []byte, flush bool) []terminalInputCommand {
	d.pending = append(d.pending, data...)
	commands := []terminalInputCommand{}
	var output []byte
	write := func() {
		if len(output) > 0 {
			commands = append(commands, terminalInputCommand{"write", append([]byte{}, output...)})
			output = nil
		}
	}
	for len(d.pending) > 0 {
		if !d.pasting && d.pending[0] == 0x1d {
			write()
			commands = append(commands, terminalInputCommand{kind: "detach"})
			d.pending = nil
			return commands
		}
		candidates := []struct{ key, kind string }{{"\x1b[200~", "paste-start"}, {"\x1b[5;2~", "history-up"}, {"\x1b[6;2~", "history-down"}}
		if d.pasting {
			candidates = []struct{ key, kind string }{{"\x1b[201~", "paste-end"}}
		}
		matched, prefix := false, false
		for _, candidate := range candidates {
			if strings.HasPrefix(string(d.pending), candidate.key) {
				if candidate.kind == "paste-start" || candidate.kind == "paste-end" {
					d.pasting = candidate.kind == "paste-start"
					output = append(output, candidate.key...)
				} else {
					write()
					commands = append(commands, terminalInputCommand{kind: candidate.kind})
				}
				d.pending = d.pending[len(candidate.key):]
				matched = true
				break
			}
			if strings.HasPrefix(candidate.key, string(d.pending)) {
				prefix = true
			}
		}
		if matched {
			continue
		}
		if prefix && !flush {
			break
		}
		output = append(output, d.pending[0])
		d.pending = d.pending[1:]
	}
	write()
	return commands
}
