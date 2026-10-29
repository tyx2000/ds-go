package tui

import (
	"bufio"
	"strings"
	"testing"
)

func pureEditor(output *strings.Builder) *LineEditor {
	return &LineEditor{cols: func() int { return 80 }, write: func(s string) { output.WriteString(s) }, width: 80}
}

func driveEditor(t *testing.T, e *LineEditor, keys string) string {
	t.Helper()
	e.in = bufio.NewReader(strings.NewReader(keys))
	e.active, e.buf, e.cursor, e.shown, e.shownCursor = true, nil, 0, nil, 0
	e.start, e.row, e.col, e.menu, e.paste = 0, 0, 1, nil, false
	e.historyIndex, e.draft, e.draftCursor = len(e.history), nil, 0
	for {
		b, err := e.in.ReadByte()
		if err != nil {
			t.Fatalf("input ended before submission: %v, draft %q", err, string(e.buf))
		}
		if line, done, err := e.key(b); done {
			if err != nil {
				t.Fatal(err)
			}
			return line
		}
	}
}

func TestLineEditorCursorDeletionWordsAndUnicode(t *testing.T) {
	for _, test := range []struct{ name, keys, want string }{
		{"CJK-middle", "你好ab\x1b[D\x1b[D中\x1b[C\x1b[3~\r", "你好中a\n"},
		{"home-end-control", "abc\x01^\x05!\x02\x04\r", "^abc\n"},
		{"word-motion", "one two three\x1b[1;5D\x1b[3~\x1b[1;5D^\x1b[F!\r", "one ^two hree!\n"},
		{"alt-word-delete", "one two\x1bb\x1bd\r", "one \n"},
		{"combining-character", "é你\x7f\x7fok\r", "ok\n"},
		{"joined-emoji", "A👩‍💻B\x1b[D\x7f\r", "AB\n"},
		{"flag-character", "A🇨🇳B\x1b[D\x7f\r", "AB\n"},
		{"paste-does-not-submit", "x\x1b[200~one\ntwo\x1b[201~\r", "xone\ntwo\n"},
		{"escape-keeps-next-key", "a\x1bx\r", "ax\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output strings.Builder
			e := pureEditor(&output)
			if got := driveEditor(t, e, test.keys); got != test.want {
				t.Fatalf("got %q want %q", got, test.want)
			}
		})
	}
}

func TestLineEditorHistoryRestoresDraftAndSkipsPrivateReads(t *testing.T) {
	var output strings.Builder
	e := pureEditor(&output)
	e.keepHistory = true
	driveEditor(t, e, "first\r")
	driveEditor(t, e, "second\r")
	if got := driveEditor(t, e, "draft\x1b[A\x1b[A\x1b[B\x1b[B\r"); got != "draft\n" {
		t.Fatal(got)
	}
	e.keepHistory = false
	if got := driveEditor(t, e, "\x1b[Asecret-answer\r"); got != "secret-answer\n" {
		t.Fatal(got)
	}
	e.keepHistory = true
	if got := driveEditor(t, e, "\x1b[A\r"); got != "draft\n" {
		t.Fatalf("private read leaked into history: %q", got)
	}
	if len(e.history) != 3 {
		t.Fatalf("duplicate history retained: %v", e.history)
	}
}

func TestLineEditorCompletionUsesRuneOffsetsAndKeepsSuffix(t *testing.T) {
	var output strings.Builder
	e := pureEditor(&output)
	e.complete = func(line string, cursor int) CompletionResult {
		if line != "你好 @f tail" || cursor != 5 {
			t.Fatalf("provider cursor %q %d", line, cursor)
		}
		return CompletionResult{Start: 3, End: 5, Candidates: []Completion{{Value: "@file一", Label: "文件一"}, {Value: "@file二", Label: "文件二"}}}
	}
	keys := "你好 @f tail\x01" + strings.Repeat("\x1b[C", 5) + "\t\x1b[B\r\x1b[F\r"
	if got := driveEditor(t, e, keys); got != "你好 @file二 tail\n" {
		t.Fatalf("completed line %q", got)
	}
	if !strings.Contains(output.String(), "> 文件二") {
		t.Fatalf("selection was not visible: %q", output.String())
	}
}

func TestLineEditorCompletionDismissAndSinglePick(t *testing.T) {
	for _, exit := range []string{"\x1b[Z", "\x1b\r"} {
		var output strings.Builder
		e := pureEditor(&output)
		e.complete = func(string, int) CompletionResult {
			return CompletionResult{Start: 0, End: 2, Candidates: []Completion{{Value: "/help "}, {Value: "/history "}}}
		}
		keys := "/h\t" + exit
		if exit == "\x1b[Z" {
			keys += "\r"
		}
		if got := driveEditor(t, e, keys); got != "/h\n" {
			t.Fatalf("dismiss changed draft: %q", got)
		}
	}
	var output strings.Builder
	e := pureEditor(&output)
	e.complete = func(string, int) CompletionResult {
		return CompletionResult{Start: 0, End: 2, Candidates: []Completion{{Value: "/help "}}}
	}
	if got := driveEditor(t, e, "/h\t\r"); got != "/help \n" {
		t.Fatal(got)
	}
}

func TestLineEditorCompletionRejectsControlsAndSanitizesLabels(t *testing.T) {
	var output strings.Builder
	e := pureEditor(&output)
	e.complete = func(string, int) CompletionResult {
		return CompletionResult{Start: 0, End: 1, Candidates: []Completion{{Value: "bad\nvalue"}, {Value: "bad\x1b]52;c;secret\a"}, {Value: "good", Label: "\x1b]52;c;secret\aGood"}, {Value: "second"}}}
	}
	if got := driveEditor(t, e, "x\t\t\r"); got != "good\n" {
		t.Fatal(got)
	}
	if strings.Contains(output.String(), "secret") || strings.Contains(output.String(), "\x1b]52") {
		t.Fatalf("completion controls reached terminal: %q", output.String())
	}
}

func TestLineEditorBackgroundStreamKeepsPartialLineAndCaret(t *testing.T) {
	var output strings.Builder
	e := pureEditor(&output)
	e.active, e.query, e.start, e.row, e.col, e.prompt = true, true, 7, 0, 10, "dsgo> "
	e.buf, e.cursor = []rune("draft"), 3
	e.shown, e.shownCursor = append([]rune(nil), e.buf...), 3
	e.WriteAbove("hello")
	if e.outputTail != "hello" || e.cursor != 3 || string(e.buf) != "draft" {
		t.Fatalf("first fragment changed draft: %q %d %q", e.outputTail, e.cursor, string(e.buf))
	}
	e.WriteAbove(" world\n")
	if e.outputTail != "" || e.cursor != 3 || string(e.buf) != "draft" {
		t.Fatalf("final fragment changed draft: %q %d %q", e.outputTail, e.cursor, string(e.buf))
	}
	if !strings.Contains(output.String(), "\x1b[1A\x1b[Jhello world\ndsgo> ") {
		t.Fatalf("fragments did not redraw as one output line: %q", output.String())
	}
}
