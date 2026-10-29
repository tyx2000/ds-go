package tui

import "strings"

func (e *LineEditor) tab() {
	e.mu.Lock()
	if e.menu != nil {
		e.acceptCompletionLocked()
		e.mu.Unlock()
		return
	}
	provider, line, cursor, revision := e.complete, string(e.buf), e.cursor, e.revision
	e.mu.Unlock()
	if provider == nil {
		return
	}
	result := provider(line, cursor)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.revision != revision || string(e.buf) != line || e.cursor != cursor {
		return
	}
	if result.Start < 0 || result.End < result.Start || result.End > len(e.buf) || cursor < result.Start || cursor > result.End {
		return
	}
	filtered := make([]Completion, 0, min(len(result.Candidates), 200))
	seen := map[string]bool{}
	for _, candidate := range result.Candidates {
		if len(filtered) >= 200 {
			break
		}
		if candidate.Value == "" || seen[candidate.Value] || Safe(candidate.Value) != candidate.Value || strings.ContainsAny(candidate.Value, "\n\r\t") {
			continue
		}
		seen[candidate.Value] = true
		candidate.Label = strings.Join(strings.Fields(Safe(candidate.Label)), " ")
		candidate.Detail = strings.Join(strings.Fields(Safe(candidate.Detail)), " ")
		if candidate.Label == "" {
			candidate.Label = candidate.Value
		}
		filtered = append(filtered, candidate)
	}
	if len(filtered) == 0 {
		return
	}
	result.Candidates = filtered
	e.menu = &completionMenu{result: result}
	if len(filtered) == 1 {
		e.acceptCompletionLocked()
	} else {
		e.renderLocked()
	}
}

func (e *LineEditor) acceptCompletionLocked() {
	if e.menu == nil {
		return
	}
	menu := e.menu
	value := []rune(menu.result.Candidates[menu.selected].Value)
	e.replaceLocked(menu.result.Start, menu.result.End, value, menu.result.Start+len(value))
}

func (e *LineEditor) dismissCompletion() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dismissCompletionLocked()
}
func (e *LineEditor) dismissCompletionLocked() {
	if e.menu != nil {
		e.menu = nil
		e.renderLocked()
	}
}

func (e *LineEditor) menuLines(cols int) []string {
	if e.menu == nil {
		return nil
	}
	menu := e.menu
	start := max(0, menu.selected-2)
	end := min(len(menu.result.Candidates), start+6)
	start = max(0, end-6)
	lines := []string{clipCells("Completions: ↑/↓ select · Tab/Enter accept · Esc close", cols)}
	for n := start; n < end; n++ {
		candidate := menu.result.Candidates[n]
		prefix := "  "
		if n == menu.selected {
			prefix = "> "
		}
		line := prefix + candidate.Label
		if candidate.Detail != "" {
			line += "  " + candidate.Detail
		}
		lines = append(lines, clipCells(line, cols))
	}
	return lines
}

func (e *LineEditor) historyMove(delta int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.historyMoveLocked(delta)
}
func (e *LineEditor) historyMoveLocked(delta int) {
	if !e.keepHistory || len(e.history) == 0 {
		return
	}
	if e.historyIndex == len(e.history) {
		if delta > 0 {
			return
		}
		e.draft = append(e.draft[:0], e.buf...)
		e.draftCursor = e.cursor
	}
	next := max(0, min(e.historyIndex+delta, len(e.history)))
	if next == e.historyIndex {
		return
	}
	e.historyIndex = next
	if next == len(e.history) {
		e.replaceLocked(0, len(e.buf), e.draft, e.draftCursor)
	} else {
		value := []rune(e.history[next])
		e.replaceLocked(0, len(e.buf), value, len(value))
	}
}
