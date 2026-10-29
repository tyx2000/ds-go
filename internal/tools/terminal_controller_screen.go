package tools

import (
	"io"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// The adapter is the only place that sees the PTY protocol. Rendered snapshots
// contain printable cells and SGR styling, with no clipboard/title/OSC effects.
type vtTerminalScreen struct {
	emulator   *vt.Emulator
	modes      map[int]bool
	closeOnce  sync.Once
	closeError error
}

func newUserTerminalScreen(cols, rows, scrollback int) terminalScreen {
	e := vt.NewEmulator(cols, rows)
	e.SetScrollbackSize(scrollback)
	s := &vtTerminalScreen{emulator: e, modes: map[int]bool{}}
	e.SetCallbacks(vt.Callbacks{EnableMode: func(mode ansi.Mode) {
		if _, ok := mode.(ansi.DECMode); ok && terminalInputMode(mode.Mode()) {
			s.modes[mode.Mode()] = true
		}
	}, DisableMode: func(mode ansi.Mode) {
		if _, ok := mode.(ansi.DECMode); ok {
			delete(s.modes, mode.Mode())
		}
	}})
	return s
}
func (s *vtTerminalScreen) Write(data []byte) error { _, err := s.emulator.Write(data); return err }
func (s *vtTerminalScreen) Resize(cols, rows int)   { s.emulator.Resize(cols, rows) }
func (s *vtTerminalScreen) Render() string          { return safeTerminalRender(s.emulator.Render()) }
func (s *vtTerminalScreen) Cursor() (int, int, bool) {
	p := s.emulator.CursorPosition()
	return p.X, p.Y, !s.emulator.CursorHidden()
}
func (s *vtTerminalScreen) Responses() io.Reader { return s.emulator }
func (s *vtTerminalScreen) Close() error {
	// Closing the pipe is concurrent-safe; Emulator.Close also mutates an
	// unsynchronized flag, which must not race its blocking response Read.
	s.closeOnce.Do(func() {
		if pipe, ok := s.emulator.InputPipe().(io.Closer); ok {
			s.closeError = pipe.Close()
		}
	})
	return s.closeError
}
func (s *vtTerminalScreen) Scrollback(offset, limit int) []string {
	end := max(0, s.emulator.ScrollbackLen()-offset)
	start := max(0, end-limit)
	lines := make([]string, 0, end-start)
	for y := start; y < end; y++ {
		var b strings.Builder
		for x := 0; x < s.emulator.Width(); x++ {
			cell := s.emulator.ScrollbackCellAt(x, y)
			if cell == nil {
				b.WriteByte(' ')
				continue
			}
			if cell.Width == 0 {
				continue
			}
			b.WriteString(cell.Style.Styled(cell.Content))
		}
		lines = append(lines, safeTerminalRender(b.String()))
	}
	return lines
}

func safeTerminalRender(value string) string {
	var out strings.Builder
	for i := 0; i < len(value); {
		if value[i] == 0x1b {
			i++
			if i == len(value) {
				break
			}
			kind := value[i]
			i++
			if kind == '[' {
				start := i
				valid := true
				for i < len(value) && !(value[i] >= 0x40 && value[i] <= 0x7e) {
					if !(value[i] >= '0' && value[i] <= '9' || value[i] == ';' || value[i] == ':') {
						valid = false
					}
					i++
				}
				if i < len(value) {
					if valid && value[i] == 'm' {
						out.WriteString("\x1b[")
						out.WriteString(value[start : i+1])
					}
					i++
				}
			} else if kind == ']' || kind == 'P' || kind == '_' || kind == '^' || kind == 'X' {
				for i < len(value) {
					if value[i] == 7 {
						i++
						break
					}
					if value[i] == 0x1b && i+1 < len(value) && value[i+1] == '\\' {
						i += 2
						break
					}
					i++
				}
			}
			continue
		}
		r, n := utf8.DecodeRuneInString(value[i:])
		i += n
		if r == '\n' {
			out.WriteByte('\n')
		} else if !unicode.IsControl(r) {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func terminalInputMode(mode int) bool {
	switch mode {
	case 1, 9, 66, 1000, 1002, 1003, 1004, 1005, 1006, 1015, 2004:
		return true
	}
	return false
}
func (s *vtTerminalScreen) InputModes() []int {
	out := []int{}
	for mode := range s.modes {
		out = append(out, mode)
	}
	sort.Ints(out)
	return out
}
