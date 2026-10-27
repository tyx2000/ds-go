package tui

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// LineEditor reads prompt lines from a terminal key by key. The kernel's
// canonical editing erases one byte and one column per backspace, which leaves
// half of a CJK character on screen and partial UTF-8 in the line. This editor
// deletes whole characters and erases their display width.
//
// Editing happens at the end of the line: Backspace, Ctrl+U (clear) and
// Ctrl+W (previous word); other control and escape sequences are ignored. The
// editor asks the terminal for the input's starting column so that deletions
// across wrapped rows redraw correctly; until that answer arrives it erases
// in place.
type LineEditor struct {
	in    *bufio.Reader
	tty   *os.File
	cols  func() int
	write func(string)

	mu     sync.Mutex
	active bool
	buf    []rune
	start  int // input start column (1-based); 0 until the terminal reports it
	asked  int // position queries not yet answered; only the latest one counts
	row    int // cursor row relative to the input's first row
	col    int // cursor column, cols+1 while the terminal defers a wrap

	restore func()
}

// NewLineEditor edits lines read from in, a buffered reader over the terminal
// tty. write must serialize with the application's other terminal output.
func NewLineEditor(in *bufio.Reader, tty, out *os.File, write func(string)) *LineEditor {
	return &LineEditor{in: in, tty: tty, write: write, cols: func() int { cols, _ := terminalSize(out); return cols }}
}

// ReadLine returns one line including its trailing newline, like
// bufio.Reader.ReadString. A terminal that cannot switch modes falls back to
// canonical reading.
func (e *LineEditor) ReadLine() (string, error) {
	restore, err := lineTerminal(e.tty)
	if err != nil {
		return e.in.ReadString('\n')
	}
	e.mu.Lock()
	e.active, e.buf, e.start, e.row, e.restore = true, e.buf[:0], 0, 0, restore
	// The report arrives in the input stream ahead of typed keys. Pasted lines
	// can return before theirs arrives, so earlier reports are discarded.
	e.asked++
	e.write("\x1b[6n")
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.active = false
		if e.restore != nil {
			e.restore()
			e.restore = nil
		}
		e.mu.Unlock()
	}()
	for {
		b, err := e.in.ReadByte()
		if err != nil {
			e.mu.Lock()
			line := string(e.buf)
			e.mu.Unlock()
			return line, err
		}
		if line, done, err := e.key(b); done {
			return line, err
		}
	}
}

// Interrupt discards the line being edited, as Ctrl+C does at a shell prompt.
func (e *LineEditor) Interrupt() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.active || len(e.buf) == 0 {
		return
	}
	e.truncate(0)
}

// Close restores the terminal if the process stops while a line is open.
func (e *LineEditor) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.restore != nil {
		e.restore()
		e.restore = nil
	}
}

func (e *LineEditor) key(b byte) (string, bool, error) {
	switch {
	case b == '\n' || b == '\r':
		if b == '\r' && e.in.Buffered() > 0 {
			if next, err := e.in.Peek(1); err == nil && next[0] == '\n' {
				_, _ = e.in.ReadByte()
			}
		}
		e.mu.Lock()
		line := string(e.buf) + "\n"
		e.write("\n")
		e.mu.Unlock()
		return line, true, nil
	case b == 0x04: // Ctrl+D ends input only on an empty line.
		e.mu.Lock()
		empty := len(e.buf) == 0
		e.mu.Unlock()
		if empty {
			return "", true, io.EOF
		}
	case b == 0x7f || b == 0x08:
		e.mu.Lock()
		if len(e.buf) > 0 {
			e.truncate(len(e.buf) - 1)
		}
		e.mu.Unlock()
	case b == 0x15:
		e.mu.Lock()
		e.truncate(0)
		e.mu.Unlock()
	case b == 0x17:
		e.mu.Lock()
		end := len(e.buf)
		for end > 0 && unicode.IsSpace(e.buf[end-1]) {
			end--
		}
		for end > 0 && !unicode.IsSpace(e.buf[end-1]) {
			end--
		}
		e.truncate(end)
		e.mu.Unlock()
	case b == 0x1b:
		e.escape()
	case b == '\t' || b >= 0x20 && b < 0x7f:
		e.insert(rune(b))
	case b >= 0x80:
		e.insertUTF8(b)
	}
	return "", false, nil
}

func (e *LineEditor) insertUTF8(lead byte) {
	p := []byte{lead}
	for !utf8.FullRune(p) && len(p) < utf8.UTFMax {
		next, err := e.in.ReadByte()
		if err != nil {
			return
		}
		p = append(p, next)
	}
	if r, size := utf8.DecodeRune(p); r != utf8.RuneError || size > 1 {
		e.insert(r)
	}
}

// escape consumes one key sequence. Only the cursor position report is used;
// a lone Escape is ignored instead of swallowing the next key.
func (e *LineEditor) escape() {
	if e.in.Buffered() == 0 {
		return
	}
	next, err := e.in.ReadByte()
	if err != nil || (next != '[' && next != 'O') {
		return
	}
	if next == 'O' {
		_, _ = e.in.ReadByte()
		return
	}
	var params []byte
	for {
		c, err := e.in.ReadByte()
		if err != nil {
			return
		}
		if c >= 0x40 && c <= 0x7e {
			if c == 'R' {
				e.report(string(params))
			}
			return
		}
		params = append(params, c)
	}
}

func (e *LineEditor) report(params string) {
	_, col, ok := strings.Cut(params, ";")
	n, err := strconv.Atoi(col)
	if !ok || err != nil || n < 1 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.asked == 0 {
		return
	}
	if e.asked--; e.asked > 0 || !e.active {
		return
	}
	// Keys typed before the report were echoed after the queried position.
	e.start = n
	e.row, e.col = layout(e.start, e.cols(), e.buf)
}

func (e *LineEditor) insert(r rune) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.buf = append(e.buf, r)
	e.write(string(r))
	if e.start > 0 {
		e.row, e.col = advance(e.row, e.col, e.cols(), r)
	}
}

// truncate shortens the line to n runes and updates the screen; mu is held.
func (e *LineEditor) truncate(n int) {
	if n >= len(e.buf) {
		return
	}
	removed := e.buf[n:]
	e.buf = e.buf[:n]
	if e.start == 0 {
		width := 0
		for _, r := range removed {
			width += runeWidth(r)
		}
		e.write(strings.Repeat("\b", width) + strings.Repeat(" ", width) + strings.Repeat("\b", width))
		return
	}
	// Redraw from the input's first cell; relative moves survive scrolling.
	var b strings.Builder
	b.WriteString("\r")
	if e.row > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", e.row)
	}
	if e.start > 1 {
		fmt.Fprintf(&b, "\x1b[%dG", e.start)
	}
	b.WriteString("\x1b[J")
	b.WriteString(string(e.buf))
	e.write(b.String())
	e.row, e.col = layout(e.start, e.cols(), e.buf)
}

// layout returns the cursor position after drawing text from column start.
func layout(start, cols int, text []rune) (int, int) {
	row, col := 0, start
	for _, r := range text {
		row, col = advance(row, col, cols, r)
	}
	return row, col
}

// advance follows terminal wrapping: a write into the last column defers the
// wrap to the next printable character, and a wide character never splits.
func advance(row, col, cols int, r rune) (int, int) {
	w := runeWidth(r)
	if r == '\t' {
		w = 8 - (col-1)%8
	}
	if w == 0 {
		return row, col
	}
	if col > cols {
		row, col = row+1, 1
	}
	if r == '\t' {
		return row, min(col+w, cols)
	}
	if col+w-1 > cols {
		row, col = row+1, 1
	}
	return row, col + w
}
