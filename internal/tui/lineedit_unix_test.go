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
	// Tests drive the position report themselves, whatever TERM the runner has.
	h.editor.query = true
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
	queries := strings.Count(h.output(), "\x1b[6n")
	go func() {
		line, err := h.editor.ReadLine()
		done <- result{line, err}
	}()
	for deadline := time.Now().Add(3 * time.Second); strings.Count(h.output(), "\x1b[6n") <= queries; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			h.t.Fatal("editor did not start a fresh read")
		}
	}
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

func TestLineEditorSubmittedDisplayKeepsRawInputAndHistory(t *testing.T) {
	h := newEditorHarness(t, 80)
	secret := "sk-1" + strings.Repeat("a", 39)
	h.editor.defaults = LineOptions{History: true, DisplaySubmitted: func(text string) string { return MaskSentUserText(text) }}
	line, err := h.read("\x1b[2;7R", secret, "\r")
	if err != nil || line != secret+"\n" {
		t.Fatalf("canonical input: %q, %v", line, err)
	}
	if got := h.output(); !strings.Contains(got, "\x1b[J"+MaskSentUserText(secret)) {
		t.Fatalf("accepted display was not redrawn: %q", got)
	}
	line, err = h.read("\x1b[3;7R", "\x1b[A", "\r")
	if err != nil || line != secret+"\n" {
		t.Fatalf("history lost the original input: %q, %v", line, err)
	}
	// Per-prompt display policy does not propagate to private questions.
	h.editor.defaults = LineOptions{}
	before := len(h.output())
	line, err = h.read("\x1b[4;7R", secret, "\r")
	if err != nil || line != secret+"\n" || strings.Contains(h.output()[before:], "***") {
		t.Fatalf("private prompt inherited chat policy: %q, %v", line, err)
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
	// Arrow keys move the caret without inserting their escape bytes. History
	// is disabled by default, so Up leaves this draft in place.
	if line, _ := h.read("\x1b[1;1R", "a\x1b[D\x1bOAb\r"); line != "ba\n" {
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

func TestLineEditorOnDumbTerminalSkipsPositionQuery(t *testing.T) {
	if _, err := exec.LookPath("stty"); err != nil {
		t.Skip("stty unavailable")
	}
	t.Setenv("TERM", "dumb")
	h := newEditorHarness(t, 80)
	h.editor = NewLineEditor(h.editor.in, h.editor.tty, h.editor.tty, h.editor.write)
	if h.editor.query {
		t.Fatal("a dumb terminal was sent a position query")
	}
	before := terminalState(t, h.editor.tty)
	done := make(chan string, 1)
	go func() { line, _ := h.editor.ReadLine(); done <- line }()
	// Without a query to wait for, wait until the line mode is in effect.
	for deadline := time.Now().Add(3 * time.Second); terminalState(t, h.editor.tty) == before; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("line mode never applied")
		}
	}
	h.master.Write([]byte("中\x7fok\r"))
	if line := <-done; line != "ok\n" {
		t.Fatalf("line %q", line)
	}
	if out := h.output(); strings.Contains(out, "\x1b") || !strings.Contains(out, "中\b\b  \b\b") {
		t.Fatalf("dumb terminal output: %q", out)
	}
}

func TestLineEditorPTYMiddleEditingAndPrivateHistory(t *testing.T) {
	h := newEditorHarness(t, 40)
	h.editor.SetHistoryEnabled(true)
	if line, _ := h.read("\x1b[1;7R", "你好ab\x1b[D\x1b[D中\x1b[C\x1b[3~\r"); line != "你好中a\n" {
		t.Fatal(line)
	}
	h.editor.SetHistoryEnabled(false)
	if line, _ := h.read("\x1b[1;7R", "\x1b[Aprivate-key\r"); line != "private-key\n" {
		t.Fatal(line)
	}
	h.editor.SetHistoryEnabled(true)
	if line, _ := h.read("\x1b[1;7R", "\x1b[A\r"); line != "你好中a\n" {
		t.Fatalf("private question entered history: %q", line)
	}
}

func TestLineEditorPTYCompletionPreservesDraftAndTerminalState(t *testing.T) {
	h := newEditorHarness(t, 40)
	before := terminalState(t, h.editor.tty)
	h.editor.SetCompletionProvider(func(line string, cursor int) CompletionResult {
		return CompletionResult{Start: 0, End: cursor, Candidates: []Completion{{Value: "/help ", Label: "帮助"}, {Value: "/history ", Label: "历史"}}}
	})
	line, err := h.read("\x1b[1;7R", "/h\t\x1b[B\r\r")
	if err != nil || line != "/history \n" {
		t.Fatalf("selection %q %v", line, err)
	}
	if !strings.Contains(h.output(), "> 历史") {
		t.Fatalf("missing menu highlight: %q", h.output())
	}
	if after := terminalState(t, h.editor.tty); after != before {
		t.Fatalf("terminal mode changed: %s -> %s", before, after)
	}
}

func TestLineEditorPTYBackgroundOutputAndResizePreserveCaret(t *testing.T) {
	h := newEditorHarness(t, 20)
	done := make(chan string, 1)
	go func() { line, _ := h.editor.ReadLineWithOptions(LineOptions{Prompt: "dsgo> "}); done <- line }()
	h.waitFor("\x1b[6n")
	h.master.Write([]byte("\x1b[1;7R你好abcd\x1b[D\x1b[D"))
	h.waitFor("你好abcd")
	// Ensure both arrow keys have reached the editor before asynchronous output.
	for deadline := time.Now().Add(time.Second); ; time.Sleep(time.Millisecond) {
		h.editor.mu.Lock()
		cursor := h.editor.cursor
		h.editor.mu.Unlock()
		if cursor == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("caret did not move")
		}
	}
	h.editor.WriteAbove("background finished\n")
	if err := pty.Setsize(h.master, &pty.Winsize{Rows: 24, Cols: 40}); err != nil {
		t.Fatal(err)
	}
	h.editor.Resize()
	h.master.Write([]byte("\x1b[1;7R中\x1b[3~\r"))
	select {
	case line := <-done:
		if line != "你好ab中d\n" {
			t.Fatalf("draft lost after output/resize: %q", line)
		}
	case <-time.After(time.Second):
		t.Fatal("line did not finish")
	}
	if !strings.Contains(h.output(), "background finished\ndsgo> ") {
		t.Fatalf("background output not above input: %q", h.output())
	}
}

func TestLineEditorPTYBracketedPasteKeepsMultilineDraft(t *testing.T) {
	h := newEditorHarness(t, 40)
	line, err := h.read("\x1b[1;7R", "\x1b[200~first\nsecond\x1b[201~\r")
	if err != nil || line != "first\nsecond\n" {
		t.Fatalf("paste submitted early: %q %v", line, err)
	}
}
