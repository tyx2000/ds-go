package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"dsgo/internal/tui"
)

// trajectoryCommand projects durable conversation activity without changing it.
// Its fullscreen mode is used only when both standard input and output are TTYs.
func (a *App) trajectoryCommand(arg string) error {
	sessionID := a.session.ID
	if sessionID == "" {
		if id, ok := a.current.Load().(string); ok {
			sessionID = id
		}
	}
	if sessionID == "" || a.store == nil {
		return errors.New("select a session before opening its trajectory")
	}
	args := strings.Fields(arg)
	plain, stats, turn := false, false, 0
	if len(args) > 0 && args[0] == "--plain" {
		plain = true
		args = args[1:]
	}
	if len(args) > 0 {
		if args[0] == "stats" {
			stats = true
			args = args[1:]
		} else {
			if args[0] == "turn" {
				args = args[1:]
			}
			if len(args) == 0 {
				return errors.New("usage: /trajectory [--plain] [turn NUMBER|NUMBER|stats]")
			}
			var err error
			turn, err = strconv.Atoi(args[0])
			if err != nil || turn < 1 {
				return errors.New("turn must be a positive number")
			}
			args = args[1:]
		}
	}
	if len(args) > 0 {
		return errors.New("usage: /trajectory [--plain] [turn NUMBER|NUMBER|stats]")
	}
	load := func() (tui.Transcript, error) {
		events, err := a.store.Events(sessionID)
		if err != nil {
			return tui.Transcript{}, err
		}
		live := a.engine != nil && a.engine.Busy(sessionID)
		return tui.Project(events, live), nil
	}
	transcript, err := load()
	if err != nil {
		return err
	}
	if turn > len(transcript.Turns) {
		return fmt.Errorf("turn %d does not exist (session has %d turns)", turn, len(transcript.Turns))
	}
	if a.flags.json {
		if stats {
			a.print(transcript.Stats)
		} else if turn > 0 {
			a.print(map[string]any{"turn": transcript.Turns[turn-1], "records": tui.SortedTurnRecords(transcript, turn)})
		} else {
			a.print(transcript)
		}
		return nil
	}
	if !plain && !stats {
		if output, ok := a.out.(*os.File); ok && tui.IsTerminal(os.Stdin) && tui.IsTerminal(output) {
			return tui.Page(a.ctx, tui.PagerOptions{Input: os.Stdin, Terminal: output, Output: trajectoryWriter{a}, Load: load, Turn: turn})
		}
	}
	var lines []string
	if stats {
		lines = tui.Statistics(transcript.Stats)
	} else {
		if turn == 0 {
			lines = append(lines, tui.Outline(transcript)...)
			lines = append(lines, "")
		}
		lines = append(lines, tui.Ledger(transcript, turn)...)
		if turn > 0 {
			for _, record := range tui.SortedTurnRecords(transcript, turn) {
				lines = append(lines, "", "────────────────────────────────────────")
				lines = append(lines, tui.Inspector(record, "summary")...)
				if record.Kind == "request" {
					lines = append(lines, tui.Inspector(record, "timing")...)
				}
			}
		}
	}
	_, err = io.WriteString(trajectoryWriter{a}, strings.Join(lines, "\n")+"\n")
	return err
}

type trajectoryWriter struct{ app *App }

func (w trajectoryWriter) Write(p []byte) (int, error) {
	w.app.writeMu.Lock()
	defer w.app.writeMu.Unlock()
	return w.app.out.Write(p)
}
