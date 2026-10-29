package tui

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Completion offsets and the provider cursor use rune indexes, never bytes.
// Value replaces [Start,End); Label and Detail are display-only text.
type Completion struct{ Value, Label, Detail string }
type CompletionResult struct {
	Start, End int
	Candidates []Completion
}
type CompletionProvider func(line string, cursor int) CompletionResult

type LineOptions struct {
	Prompt   string // already printed; retained for redraw after asynchronous output
	History  bool
	Complete CompletionProvider
	// DisplaySubmitted changes only the accepted line left on the screen.
	// The returned input and editable history retain the original text.
	DisplaySubmitted func(string) string
}

type completionMenu struct {
	result   CompletionResult
	selected int
}

// LineEditor owns terminal input and redraw. Its write callback must serialize
// raw writes, and must not call back into the editor. History is opt-in per read.
type LineEditor struct {
	in    *bufio.Reader
	tty   *os.File
	cols  func() int
	write func(string)
	query bool

	mu                            sync.Mutex
	active                        bool
	buf                           []rune
	cursor                        int
	start, asked, row, col, width int
	shown                         []rune
	shownCursor                   int
	revision                      uint64
	prompt                        string
	complete                      CompletionProvider
	displaySubmitted              func(string) string
	menu                          *completionMenu
	paste                         bool
	history                       []string
	historyIndex                  int
	draft                         []rune
	draftCursor                   int
	keepHistory                   bool
	outputTail                    string
	defaults                      LineOptions
	restore                       func()
}

func NewLineEditor(in *bufio.Reader, tty, out *os.File, write func(string)) *LineEditor {
	return &LineEditor{in: in, tty: tty, write: write, query: os.Getenv("TERM") != "dumb", cols: func() int { cols, _ := terminalSize(out); return cols }}
}

func (e *LineEditor) SetHistoryEnabled(enabled bool) {
	e.mu.Lock()
	e.defaults.History = enabled
	e.mu.Unlock()
}
func (e *LineEditor) SetCompletionProvider(provider CompletionProvider) {
	e.mu.Lock()
	e.defaults.Complete = provider
	e.mu.Unlock()
}
func (e *LineEditor) SetPrompt(prompt string) { e.mu.Lock(); e.defaults.Prompt = prompt; e.mu.Unlock() }

// ReadLine preserves the bufio.ReadString contract, including its newline.
func (e *LineEditor) ReadLine() (string, error) {
	e.mu.Lock()
	options := e.defaults
	e.mu.Unlock()
	return e.ReadLineWithOptions(options)
}

// ReadLineWithOptions captures one prompt's policy, so a cancelled stdin broker
// request cannot inherit the history or completion policy of a later question.
func (e *LineEditor) ReadLineWithOptions(options LineOptions) (string, error) {
	restore, err := lineTerminal(e.tty)
	if err != nil {
		return e.in.ReadString('\n')
	}
	e.mu.Lock()
	e.active, e.buf, e.cursor, e.start, e.row, e.col = true, e.buf[:0], 0, 0, 0, 1
	e.shown, e.shownCursor, e.width = e.shown[:0], 0, e.columns()
	e.prompt, e.complete, e.keepHistory = options.Prompt, options.Complete, options.History
	e.displaySubmitted = options.DisplaySubmitted
	e.menu, e.paste, e.restore = nil, false, restore
	e.outputTail = ""
	e.historyIndex, e.draft, e.draftCursor = len(e.history), nil, 0
	if e.query {
		e.asked++
		e.write("\x1b[?2004h\x1b[6n")
	}
	e.mu.Unlock()
	stopResize := watchLineResize(e.Resize)
	defer func() {
		stopResize()
		e.mu.Lock()
		defer e.mu.Unlock()
		e.active, e.menu, e.complete, e.paste = false, nil, nil, false
		e.displaySubmitted = nil
		if e.restore != nil {
			if e.query {
				e.write("\x1b[?2004l")
			}
			e.restore()
			e.restore = nil
		}
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

func (e *LineEditor) columns() int {
	if e.cols == nil {
		return 80
	}
	return max(1, e.cols())
}

func (e *LineEditor) Interrupt() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.active {
		return
	}
	e.menu = nil
	e.replaceLocked(0, len(e.buf), nil, 0)
}

func (e *LineEditor) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.restore != nil {
		if e.query {
			e.write("\x1b[?2004l")
		}
		e.restore()
		e.restore = nil
	}
}

// Resize reflows the visible input and menu using the current terminal width.
func (e *LineEditor) Resize() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active {
		e.renderLocked()
	}
}

// WriteAbove serializes an asynchronous message with input redraw. Callers must
// not already hold the lock acquired by the raw write callback.
func (e *LineEditor) WriteAbove(text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if text == "" {
		return
	}
	if !e.active {
		e.write(text)
		return
	}
	output := text
	if e.query && e.start > 0 && e.prompt != "" {
		var b strings.Builder
		b.WriteByte('\r')
		up := e.row
		if e.outputTail != "" {
			tailRows, _ := layout(1, e.columns(), []rune(Safe(e.outputTail)))
			up += tailRows + 1
			output = e.outputTail + text
		}
		if up > 0 {
			fmt.Fprintf(&b, "\x1b[%dA", up)
		}
		b.WriteString("\x1b[J")
		e.write(b.String())
	} else {
		e.write("\n")
	}
	e.write(output)
	if !strings.HasSuffix(output, "\n") {
		e.write("\n")
	}
	e.outputTail = output[strings.LastIndex(output, "\n")+1:]
	e.write(e.prompt)
	e.row, e.col, e.start = 0, 1, 0
	e.shown, e.shownCursor = e.shown[:0], 0
	if e.query {
		_, e.start = layout(1, e.columns(), []rune(Safe(e.prompt)))
		if e.start > e.columns() {
			e.start = 1
		}
		e.col = e.start
		e.asked++
		e.write("\x1b[6n")
	}
	e.renderLocked()
}

func (e *LineEditor) key(b byte) (string, bool, error) {
	e.mu.Lock()
	pasting := e.paste
	e.mu.Unlock()
	if pasting && b != 0x1b {
		if b == '\r' {
			b = '\n'
		}
		if b == '\n' || b == '\t' || b >= 0x20 && b < 0x7f {
			e.insert(rune(b))
		} else if b >= 0x80 {
			e.insertUTF8(b)
		}
		return "", false, nil
	}
	switch {
	case b == '\n' || b == '\r':
		if b == '\r' && e.in.Buffered() > 0 {
			if next, err := e.in.Peek(1); err == nil && next[0] == '\n' {
				_, _ = e.in.ReadByte()
			}
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.menu != nil {
			e.acceptCompletionLocked()
			return "", false, nil
		}
		if e.cursor != len(e.buf) {
			e.cursor = len(e.buf)
			e.renderLocked()
		}
		line := string(e.buf)
		if e.keepHistory && strings.TrimSpace(line) != "" && len(e.buf) <= 65536 && (len(e.history) == 0 || e.history[len(e.history)-1] != line) {
			e.history = append(e.history, line)
			if len(e.history) > 200 {
				e.history = append([]string(nil), e.history[len(e.history)-200:]...)
			}
		}
		if e.displaySubmitted != nil {
			if display := e.displaySubmitted(line); display != line {
				original, cursor := e.buf, e.cursor
				e.buf = []rune(display)
				e.cursor = len(e.buf)
				e.renderLocked()
				e.buf, e.cursor = original, cursor
			}
		}
		e.write("\n")
		e.active = false
		return line + "\n", true, nil
	case b == 0x04:
		e.mu.Lock()
		defer e.mu.Unlock()
		if len(e.buf) == 0 {
			return "", true, io.EOF
		}
		e.deleteForwardLocked()
	case b == 0x7f || b == 0x08:
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.cursor > 0 {
			start := previousBoundary(e.buf, e.cursor)
			e.replaceLocked(start, e.cursor, nil, start)
		}
	case b == 0x01:
		e.moveTo(0)
	case b == 0x05:
		e.mu.Lock()
		e.moveLocked(len(e.buf))
		e.mu.Unlock()
	case b == 0x02:
		e.mu.Lock()
		e.moveLocked(previousBoundary(e.buf, e.cursor))
		e.mu.Unlock()
	case b == 0x06:
		e.mu.Lock()
		e.moveLocked(nextBoundary(e.buf, e.cursor))
		e.mu.Unlock()
	case b == 0x10:
		e.historyMove(-1)
	case b == 0x0e:
		e.historyMove(1)
	case b == 0x15:
		e.mu.Lock()
		e.replaceLocked(0, e.cursor, nil, 0)
		e.mu.Unlock()
	case b == 0x0b:
		e.mu.Lock()
		e.replaceLocked(e.cursor, len(e.buf), nil, e.cursor)
		e.mu.Unlock()
	case b == 0x17:
		e.mu.Lock()
		start := wordLeft(e.buf, e.cursor)
		e.replaceLocked(start, e.cursor, nil, start)
		e.mu.Unlock()
	case b == '\t':
		e.tab()
	case b == 0x1b:
		e.escape()
	case b >= 0x20 && b < 0x7f:
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

func (e *LineEditor) escapeByte() (byte, error) {
	if e.in.Buffered() > 0 {
		return e.in.ReadByte()
	}
	if e.tty == nil {
		return 0, io.EOF
	}
	if err := e.tty.SetReadDeadline(time.Now().Add(40 * time.Millisecond)); err != nil {
		return 0, err
	}
	defer e.tty.SetReadDeadline(time.Time{})
	return e.in.ReadByte()
}

func (e *LineEditor) escape() {
	next, err := e.escapeByte()
	if err != nil {
		e.dismissCompletion()
		return
	}
	if next != '[' && next != 'O' {
		e.mu.Lock()
		defer e.mu.Unlock()
		switch next {
		case 'b', 'B':
			e.moveLocked(wordLeft(e.buf, e.cursor))
		case 'f', 'F':
			e.moveLocked(wordRight(e.buf, e.cursor))
		case 'd', 'D':
			e.replaceLocked(e.cursor, wordRight(e.buf, e.cursor), nil, e.cursor)
		case 0x7f, 0x08:
			start := wordLeft(e.buf, e.cursor)
			e.replaceLocked(start, e.cursor, nil, start)
		default:
			_ = e.in.UnreadByte()
			e.dismissCompletionLocked()
		}
		return
	}
	var params []byte
	for len(params) < 32 {
		c, err := e.escapeByte()
		if err != nil {
			return
		}
		if c >= 0x40 && c <= 0x7e {
			if c == 'R' {
				e.report(string(params))
				return
			}
			e.sequence(c, string(params))
			return
		}
		params = append(params, c)
	}
}

func (e *LineEditor) sequence(code byte, params string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if code == '~' && params == "200" {
		e.paste = true
		return
	}
	if code == '~' && params == "201" {
		e.paste = false
		return
	}
	if e.paste {
		return
	}
	if code == 'Z' {
		e.dismissCompletionLocked()
		return
	}
	if (code == 'A' || code == 'B') && e.menu != nil {
		delta := 1
		if code == 'A' {
			delta = -1
		}
		n := len(e.menu.result.Candidates)
		e.menu.selected = (e.menu.selected + delta + n) % n
		e.renderLocked()
		return
	}
	switch code {
	case 'A':
		e.historyMoveLocked(-1)
	case 'B':
		e.historyMoveLocked(1)
	case 'D':
		if strings.HasSuffix(params, ";5") || strings.HasSuffix(params, ";3") {
			e.moveLocked(wordLeft(e.buf, e.cursor))
		} else {
			e.moveLocked(previousBoundary(e.buf, e.cursor))
		}
	case 'C':
		if strings.HasSuffix(params, ";5") || strings.HasSuffix(params, ";3") {
			e.moveLocked(wordRight(e.buf, e.cursor))
		} else {
			e.moveLocked(nextBoundary(e.buf, e.cursor))
		}
	case 'H':
		e.moveLocked(0)
	case 'F':
		e.moveLocked(len(e.buf))
	case '~':
		switch params {
		case "1", "7":
			e.moveLocked(0)
		case "4", "8":
			e.moveLocked(len(e.buf))
		case "3":
			e.deleteForwardLocked()
		}
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
	e.start, e.width = n, e.columns()
	e.row, e.col = layout(e.start, e.width, e.buf[:e.cursor])
}

func (e *LineEditor) insert(r rune) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.buf) >= 1<<20 {
		return
	}
	if e.cursor == len(e.buf) && e.menu == nil && e.width == e.columns() {
		e.buf = append(e.buf, r)
		e.cursor++
		e.revision++
		e.write(string(r))
		if e.start > 0 {
			e.row, e.col = advance(e.row, e.col, e.width, r)
		}
		e.shown = append(e.shown, r)
		e.shownCursor = e.cursor
		return
	}
	e.replaceLocked(e.cursor, e.cursor, []rune{r}, e.cursor+1)
}

func (e *LineEditor) moveTo(cursor int) { e.mu.Lock(); defer e.mu.Unlock(); e.moveLocked(cursor) }
func (e *LineEditor) moveLocked(cursor int) {
	cursor = max(0, min(cursor, len(e.buf)))
	if cursor == e.cursor && e.menu == nil {
		return
	}
	e.cursor, e.menu = cursor, nil
	e.revision++
	e.renderLocked()
}
func (e *LineEditor) deleteForwardLocked() {
	if e.cursor < len(e.buf) {
		e.replaceLocked(e.cursor, nextBoundary(e.buf, e.cursor), nil, e.cursor)
	}
}
func (e *LineEditor) replaceLocked(start, end int, value []rune, cursor int) {
	start, end = max(0, min(start, len(e.buf))), max(0, min(end, len(e.buf)))
	if start > end {
		return
	}
	next := make([]rune, 0, len(e.buf)-(end-start)+len(value))
	next = append(next, e.buf[:start]...)
	next = append(next, value...)
	next = append(next, e.buf[end:]...)
	e.buf, e.cursor, e.menu = next, max(0, min(cursor, len(next))), nil
	e.revision++
	e.renderLocked()
}

func cells(text []rune) int {
	n := 0
	for _, r := range text {
		n += runeWidth(r)
	}
	return n
}

// renderLocked owns the complete input/menu region. Cursor offsets stay in
// characters while terminal movement uses display cells and wrapped rows.
func (e *LineEditor) renderLocked() {
	cols := e.columns()
	if e.start == 0 || !e.query {
		oldPrefix := cells(e.shown[:min(e.shownCursor, len(e.shown))])
		oldWidth := cells(e.shown)
		newWidth := cells(e.buf)
		pad := max(0, oldWidth-newWidth)
		e.write(strings.Repeat("\b", oldPrefix) + string(e.buf) + strings.Repeat(" ", pad) + strings.Repeat("\b", cells(e.buf[e.cursor:])+pad))
		if e.menu != nil {
			e.write("\n" + strings.Join(e.menuLines(cols), "\n") + "\n" + e.prompt + string(e.buf) + strings.Repeat("\b", cells(e.buf[e.cursor:])))
		}
		e.shown = append(e.shown[:0], e.buf...)
		e.shownCursor = e.cursor
		e.width = cols
		return
	}
	if cols != e.width {
		e.row, e.col = layout(e.start, cols, e.shown[:min(e.shownCursor, len(e.shown))])
	}
	var b strings.Builder
	b.WriteByte('\r')
	if e.row > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", e.row)
	}
	if e.start > 1 {
		fmt.Fprintf(&b, "\x1b[%dG", e.start)
	}
	b.WriteString("\x1b[J")
	b.WriteString(string(e.buf))
	endRow, endCol := layout(e.start, cols, e.buf)
	if endCol > cols {
		b.WriteString(" \b")
		endRow++
		endCol = 1
	}
	for _, line := range e.menuLines(cols) {
		b.WriteByte('\n')
		b.WriteString(line)
		endRow++
		endCol = cells([]rune(line)) + 1
	}
	targetRow, targetCol := layout(e.start, cols, e.buf[:e.cursor])
	if targetCol > cols {
		targetRow++
		targetCol = 1
	}
	if endRow != targetRow || endCol != targetCol {
		b.WriteByte('\r')
		if endRow > targetRow {
			fmt.Fprintf(&b, "\x1b[%dA", endRow-targetRow)
		} else if targetRow > endRow {
			fmt.Fprintf(&b, "\x1b[%dB", targetRow-endRow)
		}
		if targetCol > 1 {
			fmt.Fprintf(&b, "\x1b[%dG", targetCol)
		}
	}
	e.write(b.String())
	e.row, e.col, e.width = targetRow, targetCol, cols
	e.shown = append(e.shown[:0], e.buf...)
	e.shownCursor = e.cursor
}

func wordLeft(text []rune, pos int) int {
	for pos > 0 && unicode.IsSpace(text[pos-1]) {
		pos--
	}
	for pos > 0 && !unicode.IsSpace(text[pos-1]) {
		pos--
	}
	return pos
}
func wordRight(text []rune, pos int) int {
	for pos < len(text) && unicode.IsSpace(text[pos]) {
		pos++
	}
	for pos < len(text) && !unicode.IsSpace(text[pos]) {
		pos++
	}
	return pos
}
func extendRune(r rune) bool {
	return unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Mc, r) || unicode.Is(unicode.Me, r) || (r >= 0xfe00 && r <= 0xfe0f) || (r >= 0x1f3fb && r <= 0x1f3ff) || (r >= 0xe0100 && r <= 0xe01ef)
}
func regionalRune(r rune) bool { return r >= 0x1f1e6 && r <= 0x1f1ff }
func previousBoundary(text []rune, pos int) int {
	if pos <= 0 {
		return 0
	}
	pos--
	for pos > 0 && extendRune(text[pos]) {
		pos--
	}
	for pos > 1 && text[pos-1] == 0x200d {
		pos -= 2
		for pos > 0 && extendRune(text[pos]) {
			pos--
		}
	}
	if pos > 0 && regionalRune(text[pos]) && regionalRune(text[pos-1]) {
		count := 0
		for n := pos - 1; n >= 0 && regionalRune(text[n]); n-- {
			count++
		}
		if count%2 == 1 {
			pos--
		}
	}
	return pos
}
func nextBoundary(text []rune, pos int) int {
	if pos >= len(text) {
		return len(text)
	}
	first := pos
	pos++
	if regionalRune(text[first]) && pos < len(text) && regionalRune(text[pos]) {
		pos++
	}
	for pos < len(text) && extendRune(text[pos]) {
		pos++
	}
	for pos+1 < len(text) && text[pos] == 0x200d {
		pos += 2
		for pos < len(text) && extendRune(text[pos]) {
			pos++
		}
	}
	return pos
}

// layout follows terminal deferred wrapping; newlines in bracketed paste are
// part of the draft and do not submit it.
func layout(start, cols int, text []rune) (int, int) {
	row, col := 0, start
	for _, r := range text {
		row, col = advance(row, col, cols, r)
	}
	return row, col
}
func advance(row, col, cols int, r rune) (int, int) {
	if r == '\n' {
		return row + 1, 1
	}
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
