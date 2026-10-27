// Package tui provides terminal projections of official conversation inspection surfaces.
package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"dsgo/internal/core"
)

type Record struct {
	ID         string          `json:"id"`
	EventSeq   int64           `json:"event_seq"`
	Kind       string          `json:"kind"`
	TurnID     string          `json:"turn_id,omitempty"`
	Turn       int             `json:"turn"`
	Step       int             `json:"step,omitempty"`
	ParentID   string          `json:"parent_id,omitempty"`
	Name       string          `json:"name,omitempty"`
	Status     string          `json:"status"`
	Start      time.Time       `json:"start,omitempty"`
	End        *time.Time      `json:"end,omitempty"`
	FirstToken *time.Time      `json:"first_token,omitempty"`
	LastToken  *time.Time      `json:"last_token,omitempty"`
	Usage      *core.Usage     `json:"usage,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
	Output     json.RawMessage `json:"output,omitempty"`
	Text       string          `json:"text,omitempty"`
	Error      string          `json:"error,omitempty"`
	Images     int             `json:"images,omitempty"`
	Files      int             `json:"files,omitempty"`
}
type Turn struct {
	ID      string     `json:"id"`
	Number  int        `json:"number"`
	Title   string     `json:"title"`
	Status  string     `json:"status"`
	Start   time.Time  `json:"start,omitempty"`
	End     *time.Time `json:"end,omitempty"`
	Error   string     `json:"error,omitempty"`
	Records []int      `json:"records"`
	Usage   core.Usage `json:"usage"`
}
type Stats struct {
	Turns           int           `json:"turns"`
	Requests        int           `json:"requests"`
	FailedRequests  int           `json:"failed_requests"`
	Tools           int           `json:"tools"`
	FailedTools     int           `json:"failed_tools"`
	Subtools        int           `json:"subtools"`
	Images          int           `json:"images"`
	Files           int           `json:"files"`
	Usage           core.Usage    `json:"usage"`
	RequestDuration time.Duration `json:"request_duration"`
	ToolDuration    time.Duration `json:"tool_duration"`
	First           *time.Time    `json:"first,omitempty"`
	Last            *time.Time    `json:"last,omitempty"`
}
type Transcript struct {
	Turns   []Turn   `json:"turns"`
	Records []Record `json:"records"`
	Stats   Stats    `json:"stats"`
}

// Project reads durable facts only. Live elapsed time, TTFT and token rates are
// deliberately unavailable unless their corresponding timing facts were recorded.
func Project(events []core.Event, live bool) Transcript {
	out := Transcript{Turns: []Turn{}, Records: []Record{}}
	turns := map[string]int{}
	steps := map[string]int{}
	stepNumbers := map[string]int{}
	toolRows := map[string]int{}
	topTools := map[string]string{}
	assistantRows := map[string]int{}
	titleSet := map[string]bool{}
	add := func(r Record) int {
		index := len(out.Records)
		out.Records = append(out.Records, r)
		if ti, ok := turns[r.TurnID]; ok {
			out.Turns[ti].Records = append(out.Turns[ti].Records, index)
		}
		return index
	}
	ensureTurn := func(id string, at time.Time) int {
		if id == "" {
			return -1
		}
		if i, ok := turns[id]; ok {
			return i
		}
		i := len(out.Turns)
		turns[id] = i
		out.Turns = append(out.Turns, Turn{ID: id, Number: i + 1, Title: "Continue", Status: "running", Records: []int{}})
		return i
	}
	stepKey := func(turn string, step int) string { return turn + "/" + strconv.Itoa(step) }
	for _, ev := range events {
		ti := ensureTurn(ev.TurnID, ev.Time)
		turnNumber := 0
		if ti >= 0 {
			turnNumber = out.Turns[ti].Number
		}
		base := Record{ID: ev.ID, EventSeq: ev.Seq, TurnID: ev.TurnID, Turn: turnNumber, Step: stepNumbers[ev.TurnID], Start: ev.Time, Status: "completed"}
		switch ev.Type {
		case "turn/started":
			if ti >= 0 {
				out.Turns[ti].Start = ev.Time
			}
		case "turn/finished":
			if ti < 0 {
				continue
			}
			var p struct{ Status, Error string }
			if json.Unmarshal(ev.Data, &p) != nil {
				continue
			}
			out.Turns[ti].Status = p.Status
			out.Turns[ti].Error = p.Error
			end := ev.Time
			out.Turns[ti].End = &end
			for _, ri := range out.Turns[ti].Records {
				r := &out.Records[ri]
				if r.Status == "pending" {
					r.Status = "interrupted"
					if p.Status == "cancelled" {
						r.Status = "cancelled"
					}
					if r.Kind == "request" && p.Status == "failed" {
						r.Status = "failed"
						r.Error = p.Error
					}
				}
			}
		case "step/started":
			var p struct {
				Step  int
				Model string
			}
			if json.Unmarshal(ev.Data, &p) != nil {
				continue
			}
			stepNumbers[ev.TurnID] = p.Step
			base.Kind, base.Name, base.Status, base.Step = "request", p.Model, "pending", p.Step
			steps[stepKey(ev.TurnID, p.Step)] = add(base)
		case "step/timing":
			var p struct {
				Step      int        `json:"step"`
				Started   time.Time  `json:"started_at"`
				Completed time.Time  `json:"completed_at"`
				First     *time.Time `json:"first_token_at"`
				Last      *time.Time `json:"last_token_at"`
			}
			if json.Unmarshal(ev.Data, &p) != nil {
				continue
			}
			if ri, ok := steps[stepKey(ev.TurnID, p.Step)]; ok {
				r := &out.Records[ri]
				if !p.Started.IsZero() {
					r.Start = p.Started
				}
				if !p.Completed.IsZero() {
					r.End = &p.Completed
				}
				r.FirstToken = p.First
				r.LastToken = p.Last
			}
		case "step/usage":
			var u core.Usage
			if json.Unmarshal(ev.Data, &u) != nil {
				continue
			}
			key := stepKey(ev.TurnID, stepNumbers[ev.TurnID])
			if ri, ok := steps[key]; ok {
				v := u
				out.Records[ri].Usage = &v
			}
			if ri, ok := assistantRows[key]; ok {
				v := u
				out.Records[ri].Usage = &v
			}
			sumUsage(&out.Stats.Usage, u)
			if ti >= 0 {
				sumUsage(&out.Turns[ti].Usage, u)
			}
		case "user/message":
			var m core.Message
			if json.Unmarshal(ev.Data, &m) != nil {
				continue
			}
			base.Kind = "user"
			base.Step = 0
			base.Input = cloneJSON(ev.Data)
			base.Text, base.Images, base.Files = messageSummary(m)
			add(base)
			out.Stats.Images += base.Images
			out.Stats.Files += base.Files
			if ti >= 0 && !titleSet[ev.TurnID] {
				titleSet[ev.TurnID] = true
				title := strings.TrimSpace(strings.Join(strings.Fields(base.Text), " "))
				if title == "" {
					title = attachmentSummary(base.Images, base.Files)
				}
				if title != "" {
					out.Turns[ti].Title = clip(title, 100)
				}
			}
		case "assistant/message":
			var m core.Message
			if json.Unmarshal(ev.Data, &m) != nil {
				continue
			}
			base.Kind = "assistant"
			base.Output = cloneJSON(ev.Data)
			base.Text, base.Images, base.Files = messageSummary(m)
			key := stepKey(ev.TurnID, stepNumbers[ev.TurnID])
			if ri, ok := steps[key]; ok {
				request := &out.Records[ri]
				request.Status = "completed"
				if request.End == nil {
					end := ev.Time
					request.End = &end
				}
				base.Start = request.Start
				base.End = request.End
				base.FirstToken = request.FirstToken
				base.LastToken = request.LastToken
			}
			assistantRows[key] = add(base)
		case "assistant/attempt":
			var p struct {
				Response core.Response `json:"response"`
				Error    string        `json:"error"`
			}
			if json.Unmarshal(ev.Data, &p) != nil {
				continue
			}
			base.Kind, base.Status, base.Error = "assistant", "failed", p.Error
			base.Output = cloneJSON(ev.Data)
			base.Text, _, _ = messageSummary(core.Message{Content: p.Response.Content})
			key := stepKey(ev.TurnID, stepNumbers[ev.TurnID])
			if ri, ok := steps[key]; ok {
				r := &out.Records[ri]
				r.Status = "failed"
				r.Error = p.Error
				base.Start = r.Start
				base.End = r.End
				base.FirstToken = r.FirstToken
				base.LastToken = r.LastToken
			}
			assistantRows[key] = add(base)
		case "tool/started":
			var p struct {
				core.Block
				InputRaw string `json:"input_raw"`
			}
			if json.Unmarshal(ev.Data, &p) != nil {
				continue
			}
			base.Kind, base.Name, base.Status, base.ID = "tool", p.Name, "pending", p.ID
			base.Input = cloneJSON(p.Input)
			if p.InputRaw != "" && json.Valid([]byte(p.InputRaw)) {
				base.Input = json.RawMessage(p.InputRaw)
			}
			toolRows[ev.TurnID+"/"+p.ID] = add(base)
			topTools[ev.TurnID] = p.ID
		case "tool/subcall":
			var p struct {
				ID       json.RawMessage `json:"id"`
				CallID   string          `json:"call_id"`
				ParentID string          `json:"parent_id"`
				Name     string          `json:"name"`
				Input    json.RawMessage `json:"input"`
				InputRaw string          `json:"input_raw"`
			}
			if json.Unmarshal(ev.Data, &p) != nil {
				continue
			}
			parent := p.ParentID
			if parent == "" {
				parent = topTools[ev.TurnID]
			}
			id := p.CallID
			if id == "" {
				id = parent + "/" + wireID(p.ID)
			}
			base.Kind, base.Name, base.Status, base.ID, base.ParentID = "subtool", p.Name, "pending", id, parent
			base.Input = cloneJSON(p.Input)
			if p.InputRaw != "" && json.Valid([]byte(p.InputRaw)) {
				base.Input = json.RawMessage(p.InputRaw)
			}
			toolRows[ev.TurnID+"/"+id] = add(base)
		case "tool/finished", "tool/subresult":
			var p struct {
				ID       json.RawMessage `json:"id"`
				CallID   string          `json:"call_id"`
				ParentID string          `json:"parent_id"`
				Name     string          `json:"name"`
				Result   core.ToolResult `json:"result"`
				Error    string          `json:"error"`
			}
			if json.Unmarshal(ev.Data, &p) != nil {
				continue
			}
			id := wireID(p.ID)
			if ev.Type == "tool/subresult" {
				if p.CallID != "" {
					id = p.CallID
				} else {
					parent := p.ParentID
					if parent == "" {
						parent = topTools[ev.TurnID]
					}
					id = parent + "/" + id
				}
			}
			if ri, ok := toolRows[ev.TurnID+"/"+id]; ok {
				r := &out.Records[ri]
				r.Status = "completed"
				if p.Result.IsError || p.Error != "" {
					r.Status = "failed"
				}
				r.Error = p.Error
				if r.Error == "" && p.Result.IsError {
					r.Error = p.Result.Content
				}
				r.Output = mustJSON(p.Result)
				r.Text = p.Result.Content
				end := ev.Time
				r.End = &end
			}
		case "tool/result": // Recovery/cancellation may supply a result without a finish event.
			var m core.Message
			if json.Unmarshal(ev.Data, &m) != nil {
				continue
			}
			for _, block := range m.Content {
				if block.Type != "tool_result" {
					continue
				}
				key := ev.TurnID + "/" + block.ToolUseID
				ri, ok := toolRows[key]
				if !ok {
					for k, i := range toolRows {
						if strings.HasSuffix(k, "/"+block.ToolUseID) && out.Records[i].Kind == "tool" && out.Records[i].End == nil {
							ri, ok = i, true
							break
						}
					}
				}
				if ok && out.Records[ri].End == nil {
					r := &out.Records[ri]
					r.Output = mustJSON(block)
					r.Text = contentText(block.Content)
					if block.IsError {
						r.Status = "interrupted"
						r.Error = r.Text
					} else {
						r.Status = "completed"
					}
					if r.TurnID == ev.TurnID {
						end := ev.Time
						r.End = &end
					}
				}
			}
		case "context/compacted":
			base.Kind, base.Name = "compaction", "Context compacted"
			base.Output = cloneJSON(ev.Data)
			add(base)
		case "user/answer":
			base.Kind, base.Name = "user", "Question answered"
			base.Input = cloneJSON(ev.Data)
			add(base)
		}
	}
	// The current open tail is live only when the caller knows the engine owns it.
	for i := range out.Turns {
		t := &out.Turns[i]
		if t.Status == "running" {
			if live && i == len(out.Turns)-1 {
				t.Status = "running"
			} else {
				t.Status = "interrupted"
			}
		}
		for _, ri := range t.Records {
			r := &out.Records[ri]
			if r.Status == "pending" && t.Status != "running" {
				r.Status = "interrupted"
			}
		}
	}
	out.Stats.Turns = len(out.Turns)
	for _, r := range out.Records {
		if !r.Start.IsZero() {
			v := r.Start
			if out.Stats.First == nil || v.Before(*out.Stats.First) {
				out.Stats.First = &v
			}
		}
		if r.End != nil {
			v := *r.End
			if out.Stats.Last == nil || v.After(*out.Stats.Last) {
				out.Stats.Last = &v
			}
		}
		switch r.Kind {
		case "request":
			out.Stats.Requests++
			if r.Status == "failed" || r.Status == "interrupted" || r.Status == "cancelled" {
				out.Stats.FailedRequests++
			}
			if d, ok := r.Duration(); ok {
				out.Stats.RequestDuration += d
			}
		case "tool", "subtool":
			out.Stats.Tools++
			if r.Kind == "subtool" {
				out.Stats.Subtools++
			}
			if r.Status == "failed" || r.Status == "interrupted" || r.Status == "cancelled" {
				out.Stats.FailedTools++
			}
			if d, ok := r.Duration(); ok {
				out.Stats.ToolDuration += d
			}
		}
	}
	return out
}
func (r Record) Duration() (time.Duration, bool) {
	if r.Start.IsZero() || r.End == nil || r.End.Before(r.Start) {
		return 0, false
	}
	return r.End.Sub(r.Start), true
}
func (r Record) TTFT() (time.Duration, bool) {
	if r.Start.IsZero() || r.FirstToken == nil || r.FirstToken.Before(r.Start) {
		return 0, false
	}
	return r.FirstToken.Sub(r.Start), true
}
func (r Record) Generation() (time.Duration, bool) {
	if r.FirstToken == nil || r.LastToken == nil || r.LastToken.Before(*r.FirstToken) {
		return 0, false
	}
	return r.LastToken.Sub(*r.FirstToken), true
}
func (r Record) TokensPerSecond() (float64, bool) {
	d, ok := r.Generation()
	if !ok || d <= 0 || r.Usage == nil || r.Usage.OutputTokens <= 0 {
		return 0, false
	}
	rate := float64(r.Usage.OutputTokens) / d.Seconds()
	if math.IsInf(rate, 0) || math.IsNaN(rate) {
		return 0, false
	}
	return rate, true
}
func sumUsage(a *core.Usage, b core.Usage) {
	a.InputTokens += b.InputTokens
	a.OutputTokens += b.OutputTokens
	a.CacheReadTokens += b.CacheReadTokens
	a.CacheWriteTokens += b.CacheWriteTokens
}
func messageSummary(m core.Message) (string, int, int) {
	var text, thinking strings.Builder
	images, files := 0, 0
	for _, b := range m.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "thinking":
			thinking.WriteString(b.Thinking)
		case "image":
			images++
		case "file", "document":
			files++
		}
	}
	if text.Len() == 0 {
		return thinking.String(), images, files
	}
	return text.String(), images, files
}

func attachmentSummary(images, files int) string {
	var parts []string
	if images > 0 {
		parts = append(parts, fmt.Sprintf("%d images", images))
	}
	if files > 0 {
		parts = append(parts, fmt.Sprintf("%d files", files))
	}
	return strings.Join(parts, ", ")
}
func wireID(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return string(raw)
}
func contentText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return string(mustJSON(v))
}
func mustJSON(v any) json.RawMessage              { b, _ := json.Marshal(v); return b }
func cloneJSON(v json.RawMessage) json.RawMessage { return bytes.Clone(v) }
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// Safe removes terminal control sequences, including OSC hyperlinks and C1 controls.
// Model text may never change the terminal title, clipboard, screen, or input mode.
func Safe(s string) string {
	var out strings.Builder
	escape := false
	csi := false
	osc := false
	for _, r := range s {
		if osc {
			if r == '\a' {
				osc = false
				escape = false
			} else if escape && r == '\\' {
				osc = false
				escape = false
			} else {
				escape = r == '\x1b'
			}
			continue
		}
		if csi {
			if r >= 0x40 && r <= 0x7e {
				csi = false
			}
			continue
		}
		if escape {
			escape = false
			switch r {
			case '[':
				csi = true
			case ']', 'P', 'X', '^', '_':
				osc = true
			}
			continue
		}
		if r == '\x1b' {
			escape = true
			continue
		}
		if r == 0x9b {
			csi = true
			continue
		}
		if r == 0x9d {
			osc = true
			continue
		}
		if r == '\n' || r == '\t' {
			out.WriteRune(r)
		} else if !unicode.IsControl(r) && r != 0x7f {
			out.WriteRune(r)
		}
	}
	return out.String()
}
func SortedTurnRecords(t Transcript, turn int) []Record {
	out := []Record{}
	for _, r := range t.Records {
		if turn == 0 || r.Turn == turn {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].EventSeq < out[j].EventSeq })
	return out
}
