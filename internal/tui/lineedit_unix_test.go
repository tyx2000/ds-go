//go:build darwin || linux

package tui

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

type editorHarness struct {
	t      *testing.T
	master *os.File
	editor *LineEditor
	mu     sync.Mutex
	shown  strings.Builder
}

func newEditorHarness(t *testing.T, cols uint16) *editorHarness {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close(); slave.Close() })
	if err = pty.Setsize(master, &pty.Winsize{Rows: 24, Cols: cols}); err != nil {
		t.Fatal(err)
	}
	h := &editorHarness{t: t, master: master}
	h.editor = NewLineEditor(bufio.NewReader(slave), slave, slave, func(s string) {
		h.mu.Lock()
		h.shown.WriteString(s)
		h.mu.Unlock()
	})
	return h
}

func (h *editorHarness) output() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.shown.String()
}

// waitFor blocks until the editor has written want, which also proves the
// terminal is already in line mode before keys are typed.
func (h *editorHarness) waitFor(want string) {
	h.t.Helper()
	for deadline := time.Now().Add(3 * time.Second); !strings.Contains(h.output(), want); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			h.t.Fatalf("editor never wrote %q; got %q", want, h.output())
		}
	}
}

func (h *editorHarness) read(keys ...string) (string, error) {
	h.t.Helper()
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := h.editor.ReadLine()
		done <- result{line, err}
	}()
	h.waitFor("\x1b[6n")
	for _, k := range keys {
		if _, err := h.master.Write([]byte(k)); err != nil {
			h.t.Fatal(err)
		}
	}
	select {
	case r := <-done:
		return r.line, r.err
	case <-time.After(3 * time.Second):
		h.t.Fatalf("line did not finish; screen %q", h.output())
		return "", nil
	}
}

func terminalState(t *testing.T, tty *os.File) string {
	t.Helper()
	cmd := exec.Command("stty", "-g")
	cmd.Stdin = tty
	data, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestLineEditorDeletesWholeWideCharactersAndRestoresTerminal(t *testing.T) {
	if _, err := exec.LookPath("stty"); err != nil {
		t.Skip("stty unavailable")
	}
	h := newEditorHarness(t, 80)
	before := terminalState(t, h.editor.tty)
	// The position report says the prompt "dsgo> " ends at column 7.
	line, err := h.read("\x1b[3;7R", "你好a", "\x7f\x7f", "x\r")
	if err != nil || line != "你x\n" {
		t.Fatalf("line %q, %v", line, err)
	}
	if !strings.Contains(h.output(), "\r\x1b[7G\x1b[J你") {
		t.Fatalf("deletion did not redraw from the input start: %q", h.output())
	}
	if after := terminalState(t, h.editor.tty); after != before {
		t.Fatalf("terminal mode not restored:\n%s\n%s", before, after)
	}
}

func TestLineEditorRedrawsAcrossWrappedRows(t *testing.T) {
	h := newEditorHarness(t, 20)
	// Four wide characters from column 15: two fit, the rest wrap to row 2.
	line, err := h.read("\x1b[5;15R", "你好你好", "\x7f", "\r")
	if err != nil || line != "你好你\n" {
		t.Fatalf("line %q, %v", line, err)
	}
	if !strings.Contains(h.output(), "\r\x1b[1A\x1b[15G\x1b[J你好你") {
		t.Fatalf("wrapped deletion did not return to the first row: %q", h.output())
	}
}

func TestLineEditorErasesInPlaceWithoutPositionReport(t *testing.T) {
	h := newEditorHarness(t, 80)
	line, err := h.read("中", "\x7f", "ok\r")
	if err != nil || line != "ok\n" {
		t.Fatalf("line %q, %v", line, err)
	}
	if !strings.Contains(h.output(), "中\b\b  \b\b") {
		t.Fatalf("wide character not erased by its width: %q", h.output())
	}
}

func TestLineEditorControlKeysAndInterrupt(t *testing.T) {
	h := newEditorHarness(t, 80)
	if line, _ := h.read("\x1b[1;1R", "abc def", "\x17", "\x15", "kept\r"); line != "kept\n" {
		t.Fatalf("Ctrl+W/Ctrl+U: %q", line)
	}
	if line, _ := h.read("\x1b[1;1R", "one two  ", "\x17", "\r"); line != "one \n" {
		t.Fatalf("Ctrl+W removes the previous word and its trailing spaces: %q", line)
	}
	// Arrow keys and other escape sequences never reach the line.
	if line, _ := h.read("\x1b[1;1R", "a\x1b[D\x1bOAb\r"); line != "ab\n" {
		t.Fatalf("escape sequences leaked: %q", line)
	}
	if _, err := h.read("\x1b[1;1R", "\x04"); err != io.EOF {
		t.Fatalf("Ctrl+D on an empty line: %v", err)
	}
	done := make(chan string, 1)
	go func() { line, _ := h.editor.ReadLine(); done <- line }()
	h.waitFor("\x1b[6n")
	h.master.Write([]byte("\x1b[1;1Rdraft"))
	h.waitFor("draft")
	h.editor.Interrupt()
	h.master.Write([]byte("fresh\r"))
	if line := <-done; line != "fresh\n" {
		t.Fatalf("Ctrl+C did not discard the typed line: %q", line)
	}
}

func TestLayoutFollowsTerminalWrapping(t *testing.T) {
	for _, c := range []struct {
		start, cols int
		text        string
		row, col    int
	}{
		{7, 10, "你好", 0, 11}, // filling the last column defers the wrap
		{7, 10, "你好a", 1, 2}, // the next character wraps first
		{10, 10, "你", 1, 3},  // a wide character never splits across rows
		{1, 10, "é", 0, 2},  // combining marks take no column
	} {
		if row, col := layout(c.start, c.cols, []rune(c.text)); row != c.row || col != c.col {
			t.Fatalf("%q from %d in %d columns: got %d,%d want %d,%d", c.text, c.start, c.cols, row, col, c.row, c.col)
		}
	}
}
