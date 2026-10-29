package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"dsgo/internal/core"
)

func Outline(t Transcript) []string {
	out := []string{"Turn outline"}
	for _, turn := range t.Turns {
		out = append(out, fmt.Sprintf("%3d  %-11s %s  %s", turn.Number, turn.Status, shortID(turn.ID), Safe(turn.Title)))
	}
	if len(t.Turns) == 0 {
		out = append(out, "No recorded turns.")
	}
	return out
}
func Ledger(t Transcript, turn int) []string {
	out := []string{}
	lastTurn := -1
	for _, r := range t.Records {
		if turn > 0 && r.Turn != turn {
			continue
		}
		if r.Turn != lastTurn {
			lastTurn = r.Turn
			if r.Turn == 0 {
				out = append(out, "Between turns")
			} else {
				tr := t.Turns[r.Turn-1]
				out = append(out, fmt.Sprintf("Turn %d · %s · %s", tr.Number, tr.Status, Safe(tr.Title)))
			}
		}
		out = append(out, RecordLine(r))
	}
	if len(out) == 0 {
		out = append(out, "No recorded activity.")
	}
	return out
}
func RecordLine(r Record) string {
	indent := ""
	if r.Kind == "subtool" {
		indent = "  └ "
	}
	label := r.Name
	if label == "" {
		label = r.Text
	}
	label = clip(strings.Join(strings.Fields(Safe(label)), " "), 100)
	duration := "—"
	if d, ok := r.Duration(); ok {
		duration = formatDuration(d)
	}
	step := ""
	if r.Step > 0 {
		step = fmt.Sprintf(" step %d", r.Step)
	}
	media := attachmentSummary(r.Images, r.Files)
	if media != "" {
		media = " [" + media + "]"
	}
	return fmt.Sprintf("%s%-10s %-11s %8s%s  %s%s", indent, r.Kind, r.Status, duration, step, label, media)
}
func Statistics(s Stats) []string {
	out := []string{"Session statistics", fmt.Sprintf("Turns: %d  Requests: %d  Failed/interrupted requests: %d", s.Turns, s.Requests, s.FailedRequests), fmt.Sprintf("Tool calls: %d (nested: %d)  Failed/interrupted tools: %d", s.Tools, s.Subtools, s.FailedTools), fmt.Sprintf("Attachments: %d images, %d files", s.Images, s.Files), "", usageLine(s.Usage), fmt.Sprintf("Recorded request duration (sum): %s", formatDuration(s.RequestDuration)), fmt.Sprintf("Recorded tool duration (sum, calls can overlap): %s", formatDuration(s.ToolDuration))}
	if s.First != nil && s.Last != nil && !s.Last.Before(*s.First) {
		out = append(out, fmt.Sprintf("Recorded activity span: %s", formatDuration(s.Last.Sub(*s.First))))
	}
	out = append(out, "Unavailable timing, token rates and prices are not estimated.")
	return out
}
func Inspector(r Record, tab string) []string {
	return inspectRecord(r, tab, nil)
}

func inspectRecord(r Record, tab string, revealed map[string]bool) []string {
	if tab != "raw" {
		r = displayUserRecord(r, revealed)
	}
	out := []string{fmt.Sprintf("%s · %s · %s", r.Kind, Safe(r.Name), r.Status), fmt.Sprintf("Turn %d / step %d · event %d · %s", r.Turn, r.Step, r.EventSeq, Safe(r.ID))}
	if r.ParentID != "" {
		out = append(out, "Parent tool: "+Safe(r.ParentID))
	}
	if r.Error != "" {
		out = append(out, "Error: "+Safe(r.Error))
	}
	out = append(out, "")
	switch tab {
	case "timing":
		out = append(out, "Timing", "Started: "+clock(r.Start))
		if r.End != nil {
			out = append(out, "Completed: "+clock(*r.End))
		}
		if d, ok := r.Duration(); ok {
			out = append(out, "Duration: "+formatDuration(d))
		} else {
			out = append(out, "Duration: unavailable")
		}
		if d, ok := r.TTFT(); ok {
			out = append(out, "TTFT: "+formatDuration(d))
		} else {
			out = append(out, "TTFT: unavailable")
		}
		if d, ok := r.Generation(); ok {
			out = append(out, "Generation: "+formatDuration(d))
		} else {
			out = append(out, "Generation: unavailable")
		}
		if rate, ok := r.TokensPerSecond(); ok {
			out = append(out, fmt.Sprintf("Output: %.2f tokens/s", rate))
		} else {
			out = append(out, "Output rate: unavailable")
		}
		if r.Usage != nil {
			out = append(out, usageLine(*r.Usage))
		}
	case "raw":
		out = append(out, "Recorded input (original JSON whitespace)")
		out = append(out, strings.Split(Safe(string(r.Input)), "\n")...)
		out = append(out, "", "Recorded output")
		out = append(out, strings.Split(Safe(string(r.Output)), "\n")...)
	case "input":
		out = append(out, "Input")
		out = append(out, prettyLines(r.Input)...)
	case "code":
		var p struct {
			Code string `json:"code"`
		}
		if r.Name == "run_code" && json.Unmarshal(r.Input, &p) == nil {
			out = append(out, "Code (line numbers are presentation only)")
			for i, line := range strings.Split(p.Code, "\n") {
				out = append(out, fmt.Sprintf("%4d  %s", i+1, Safe(line)))
			}
		} else {
			out = append(out, "No code body in this record.")
		}
	case "output":
		out = append(out, "Output")
		out = append(out, prettyLines(r.Output)...)
	default:
		if r.Text != "" {
			out = append(out, strings.Split(Safe(r.Text), "\n")...)
		}
		if r.Images > 0 || r.Files > 0 {
			out = append(out, "", attachmentSummary(r.Images, r.Files), "Attachment metadata is available in Raw.")
		}
		out = append(out, "", "Input")
		out = append(out, prettyLines(r.Input)...)
		out = append(out, "", "Output")
		out = append(out, prettyLines(r.Output)...)
	}
	return out
}
func prettyLines(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return []string{"(unavailable)"}
	}
	var obj any
	if json.Unmarshal(raw, &obj) == nil {
		if s, ok := obj.(string); ok {
			return strings.Split(Safe(s), "\n")
		}
		b, e := json.MarshalIndent(obj, "", "  ")
		if e == nil {
			return strings.Split(Safe(string(b)), "\n")
		}
	}
	return strings.Split(Safe(string(raw)), "\n")
}
func usageLine(u core.Usage) string {
	return fmt.Sprintf("Tokens: input %d · output %d · cache read %d · cache write %d", u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
}
func formatDuration(d time.Duration) string {
	if d < time.Millisecond {
		return d.String()
	}
	if d < time.Second {
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}
func clock(t time.Time) string {
	if t.IsZero() {
		return "unavailable"
	}
	return t.Local().Format("2006-01-02 15:04:05.000 MST")
}
func shortID(id string) string {
	if len(id) > 10 {
		return id[:10]
	}
	return id
}
func Wrap(lines []string, width int) []string {
	if width < 1 {
		width = 1
	}
	out := []string{}
	for _, line := range lines {
		line = Safe(line)
		var part strings.Builder
		col := 0
		flush := func() { out = append(out, part.String()); part.Reset(); col = 0 }
		for _, r := range line {
			if r == '\n' {
				flush()
				continue
			}
			if r == '\t' {
				spaces := 4 - col%4
				for i := 0; i < spaces; i++ {
					if col >= width {
						flush()
					}
					part.WriteByte(' ')
					col++
				}
				continue
			}
			w := runeWidth(r)
			if col+w > width && col > 0 {
				flush()
			}
			part.WriteRune(r)
			col += w
		}
		flush()
	}
	return out
}

// Terminals commonly render East Asian characters and emoji as two cells.
// Combining marks and variation selectors occupy no additional cells.
func runeWidth(r rune) int {
	if (r >= 0x300 && r <= 0x36f) || (r >= 0xfe00 && r <= 0xfe0f) || r == 0x200d {
		return 0
	}
	if r >= 0x1100 && ((r <= 0x115f) || (r >= 0x2329 && r <= 0x232a) || (r >= 0x2e80 && r <= 0xa4cf) || (r >= 0xac00 && r <= 0xd7a3) || (r >= 0xf900 && r <= 0xfaff) || (r >= 0xfe10 && r <= 0xfe19) || (r >= 0xfe30 && r <= 0xfe6f) || (r >= 0xff00 && r <= 0xff60) || (r >= 0xffe0 && r <= 0xffe6) || (r >= 0x1f000 && r <= 0x1faff) || (r >= 0x20000 && r <= 0x3ffff)) {
		return 2
	}
	return 1
}
