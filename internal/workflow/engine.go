package workflow

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

//go:embed runtime.js
var runtimeSource string

type Engine struct {
	mu      sync.Mutex
	options Options
	runs    map[string]*Run
	closed  bool
	wg      sync.WaitGroup
}
type Run struct {
	ID       string
	Meta     Meta
	ParentID string
	started  time.Time
	ended    time.Time
	mu       sync.Mutex
	result   Result
	events   []Event
	members  map[int]Event
	done     chan struct{}
	cancel   context.CancelFunc
	reason   string
	observer func(Event)
	cursor   int
}
type Snapshot struct {
	ID        string    `json:"id"`
	ParentID  string    `json:"parentId"`
	Meta      Meta      `json:"meta"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"startedAt"`
	EndedAt   time.Time `json:"endedAt,omitempty"`
	Events    []Event   `json:"events"`
	Result    *Result   `json:"result,omitempty"`
}

func New(o Options) *Engine { o.defaults(); return &Engine{options: o, runs: map[string]*Run{}} }
func (e *Engine) Start(ctx context.Context, req Request) (*Run, error) {
	if req.WorkspaceRoot == "" {
		req.WorkspaceRoot = req.CWD
	}
	if req.Permission == "" {
		req.Permission = "workspace-write"
	}
	if req.ParentID == "" {
		return nil, errors.New("workflow requires a calling agent")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m, err := ValidateMeta(mustJSON(req.Meta))
	if err != nil {
		return nil, err
	}
	req.Meta = m
	if len(req.Script) > 8<<20 {
		return nil, errors.New("SCRIPT_PARSE: script exceeds size limit")
	}
	if strings.HasPrefix(strings.TrimSpace(req.Script), "export const meta") {
		return nil, errors.New("SCRIPT_PARSE: meta belongs in the meta request field, not the script")
	}
	node, err := exec.LookPath(e.options.NodePath)
	if err != nil {
		return nil, errors.New("workflow requires Node.js on PATH")
	}
	if e.options.Command == nil || e.options.Child == nil {
		return nil, errors.New("workflow runtime is not configured")
	}
	// Compile without running before publishing the run, as the official engine does.
	parseCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	cmd, cleanup, err := e.options.Command(parseCtx, req.CWD, req.WorkspaceRoot, req.Permission, node, "--max-old-space-size=512", "--no-warnings", "-e", `const fs=require('node:fs'),vm=require('node:vm');new vm.Script('(async()=>{\n'+fs.readFileSync(0,'utf8')+'\n})()');`)
	if err != nil {
		cancel()
		return nil, err
	}
	cmd.Stdin = strings.NewReader(req.Script)
	var parseOut limitedBuffer
	cmd.Stderr = &parseOut
	err = cmd.Run()
	cleanup()
	cancel()
	if err != nil {
		return nil, fmt.Errorf("SCRIPT_PARSE: %s", strings.TrimSpace(parseOut.String()))
	}
	idBytes := make([]byte, 16)
	if _, err = rand.Read(idBytes); err != nil {
		return nil, err
	}
	runctx, cancelRun := context.WithCancel(ctx)
	r := &Run{ID: "workflow-" + hex.EncodeToString(idBytes), Meta: m, ParentID: req.ParentID, started: time.Now().UTC(), done: make(chan struct{}), cancel: cancelRun, observer: req.Observe, members: map[int]Event{}}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		cancelRun()
		return nil, errors.New("workflow engine closing")
	}
	e.runs[r.ID] = r
	e.wg.Add(1)
	e.mu.Unlock()
	r.emit(Event{Type: "run-start", Name: m.Name})
	go func() { defer e.wg.Done(); defer cancelRun(); r.drive(runctx, e.options, req, node) }()
	return r, nil
}
func (r *Run) Done() <-chan struct{} { return r.done }
func (r *Run) Wait(ctx context.Context) (Result, error) {
	select {
	case <-r.done:
		r.mu.Lock()
		v := clone(r.result)
		r.mu.Unlock()
		return v, nil
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}
func (r *Run) Cancel(reason string) {
	r.mu.Lock()
	if r.reason == "" {
		r.reason = reason
	}
	r.mu.Unlock()
	r.cancel()
}
func (r *Run) Dispose() { r.Cancel("workflow disposed"); <-r.done }
func (e *Engine) Close() {
	e.mu.Lock()
	e.closed = true
	var runs []*Run
	for _, r := range e.runs {
		runs = append(runs, r)
	}
	e.mu.Unlock()
	for _, r := range runs {
		r.Cancel("owner teardown")
	}
	e.wg.Wait()
}
func (e *Engine) Get(owner, id string) (*Run, error) {
	e.mu.Lock()
	r := e.runs[id]
	e.mu.Unlock()
	if r == nil || r.ParentID != owner {
		return nil, errors.New("workflow job does not exist in this session")
	}
	return r, nil
}
func (e *Engine) List(owner string) []Snapshot {
	e.mu.Lock()
	var runs []*Run
	for _, r := range e.runs {
		if r.ParentID == owner {
			runs = append(runs, r)
		}
	}
	e.mu.Unlock()
	out := []Snapshot{}
	for _, r := range runs {
		out = append(out, r.Snapshot())
	}
	return out
}
func (r *Run) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Snapshot{ID: r.ID, ParentID: r.ParentID, Meta: clone(r.Meta), Status: "running", StartedAt: r.started, EndedAt: r.ended, Events: clone(r.events)}
	if !r.ended.IsZero() {
		v := clone(r.result)
		s.Result = &v
		s.Status = v.StopReason
	}
	return s
}
func (r *Run) emit(ev Event) {
	ev.RunID = r.ID
	r.mu.Lock()
	r.events = append(r.events, clone(ev))
	observe := r.observer
	r.mu.Unlock()
	if observe != nil {
		func() { defer func() { _ = recover() }(); observe(clone(ev)) }()
	}
}
func (r *Run) memberStart(req ChildRequest, id string) {
	ev := Event{Type: "agent-start", Seq: req.Seq, Label: req.Label, Phase: req.Phase, ChildID: id}
	r.mu.Lock()
	r.members[req.Seq] = ev
	r.mu.Unlock()
	r.emit(ev)
}
func (r *Run) memberEnd(seq int, outcome string) {
	r.mu.Lock()
	ev, ok := r.members[seq]
	if ok {
		delete(r.members, seq)
	}
	r.mu.Unlock()
	if ok {
		ev.Type = "agent-end"
		ev.Outcome = outcome
		r.emit(ev)
	}
}
func (r *Run) drive(ctx context.Context, o Options, req Request, node string) {
	result := Result{StopReason: "error", Error: "workflow process failed", Value: nil}
	// Every terminal path waits for all admitted child callbacks before ending.
	var children sync.WaitGroup
	childCtx, cancelChildren := context.WithCancel(ctx)
	defer func() {
		cancelChildren()
		children.Wait()
		r.mu.Lock()
		var remaining []int
		for seq := range r.members {
			remaining = append(remaining, seq)
		}
		r.mu.Unlock()
		for _, seq := range remaining {
			r.memberEnd(seq, "cancelled")
		}
		if ctx.Err() != nil {
			r.mu.Lock()
			reason := r.reason
			r.mu.Unlock()
			if reason == "" {
				reason = ctx.Err().Error()
			}
			result = Result{Value: nil, StopReason: "cancelled", Error: reason, AgentsStarted: result.AgentsStarted}
		}
		r.mu.Lock()
		r.result = clone(result)
		r.ended = time.Now().UTC()
		r.mu.Unlock()
		r.emit(Event{Type: "run-end", StopReason: result.StopReason})
		close(r.done)
	}()
	cmd, cleanup, err := o.Command(ctx, req.CWD, req.WorkspaceRoot, req.Permission, node, "--max-old-space-size=512", "--no-warnings", "-e", runtimeSource)
	if err != nil {
		result.Error = err.Error()
		return
	}
	defer cleanup()
	reader, writer, err := os.Pipe()
	if err != nil {
		result.Error = err.Error()
		return
	}
	defer reader.Close()
	defer writer.Close()
	cmd.ExtraFiles = []*os.File{writer}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		result.Error = err.Error()
		return
	}
	defer stdin.Close()
	var logs limitedBuffer
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if err = cmd.Start(); err != nil {
		result.Error = err.Error()
		return
	}
	writer.Close()
	var writeMu sync.Mutex
	reply := func(v any) { writeMu.Lock(); defer writeMu.Unlock(); _, _ = stdin.Write(append(mustJSON(v), '\n')) }
	reply(map[string]any{"body": req.Script, "meta": req.Meta, "args": req.Args, "concurrent": o.MaxConcurrentAgents, "maxTotal": o.MaxTotalAgents, "maxItems": o.MaxItemsPerCall, "syncTimeout": o.SyncTimeoutMS})
	scan := bufio.NewScanner(reader)
	scan.Buffer(make([]byte, 65536), 8<<20)
	gotResult := false
	for scan.Scan() {
		var msg struct {
			Type    string       `json:"type"`
			ID      int          `json:"id"`
			Title   string       `json:"title"`
			Message string       `json:"message"`
			Request ChildRequest `json:"request"`
			Result  Result       `json:"result"`
		}
		if err = json.Unmarshal(scan.Bytes(), &msg); err != nil {
			result.Error = "invalid workflow process frame"
			r.cancel()
			break
		}
		switch msg.Type {
		case "phase":
			r.emit(Event{Type: "phase", Message: msg.Title})
		case "log":
			r.emit(Event{Type: "log", Message: msg.Message})
		case "agent":
			if msg.ID <= 0 || msg.Request.Seq != msg.ID || msg.Request.Prompt == "" {
				result.Error = "invalid workflow child request"
				r.cancel()
				break
			}
			result.AgentsStarted++
			children.Add(1)
			go func(id int, cr ChildRequest) {
				defer children.Done()
				var published bool
				value, callErr := o.Child(childCtx, req.ParentID, cr, func(childID string) { published = true; r.memberStart(cr, childID) })
				outcome := "completed"
				if callErr != nil || value.StopReason != "completed" {
					outcome = "failed"
				}
				if childCtx.Err() != nil {
					outcome = "cancelled"
				}
				if published {
					r.memberEnd(cr.Seq, outcome)
				}
				if callErr != nil {
					reply(map[string]any{"id": id, "error": callErr.Error()})
				} else {
					reply(map[string]any{"id": id, "value": value})
				}
			}(msg.ID, msg.Request)
		case "result":
			result = msg.Result
			gotResult = true
		default:
			result.Error = "invalid workflow process frame type"
			r.cancel()
		}
	}
	cancelChildren()
	children.Wait()
	stdin.Close()
	err = cmd.Wait()
	if !gotResult {
		if scan.Err() != nil {
			result.Error = "workflow transport: " + scan.Err().Error()
		} else if err != nil {
			result.Error = "workflow process: " + err.Error()
			if s := logs.String(); s != "" {
				result.Error += " " + s
			}
		}
	}
}
func Render(name string, r Result, maxChars int) string {
	b, _ := json.MarshalIndent(r.Value, "", "  ")
	text := string(b)
	chars := []rune(text)
	if len(chars) > maxChars {
		text = string(chars[:maxChars]) + fmt.Sprintf("\n… [truncated: %d more characters]", len(chars)-maxChars)
	}
	plural := "s"
	if r.AgentsStarted == 1 {
		plural = ""
	}
	return fmt.Sprintf("workflow %q completed (%d agent%s).\nReturn value:\n%s", name, r.AgentsStarted, plural, text)
}
func (r *Run) JobSnapshot(offset int, consume bool) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out strings.Builder
	for _, ev := range r.events {
		if ev.Type == "log" || ev.Type == "phase" {
			fmt.Fprintln(&out, ev.Message)
		} else if ev.Type == "agent-start" || ev.Type == "agent-end" {
			fmt.Fprintf(&out, "[%s] %s %s\n", ev.Type, ev.Label, ev.Outcome)
		}
	}
	status := "running"
	if !r.ended.IsZero() {
		status = r.result.StopReason
		if status == "completed" {
			out.WriteString(Render(r.Meta.Name, r.result, 50000))
		} else {
			out.WriteString(r.result.Error)
		}
	}
	text := out.String()
	if consume && offset < r.cursor {
		offset = r.cursor
	}
	if offset > len(text) {
		offset = len(text)
	}
	if consume {
		r.cursor = len(text)
	}
	return map[string]any{"id": r.ID, "kind": "workflow", "owner": r.ParentID, "label": r.Meta.Name, "status": status, "output": text[offset:], "offset": offset, "next_offset": len(text)}
}
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func clone[T any](v T) T    { var out T; _ = json.Unmarshal(mustJSON(v), &out); return out }

type limitedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if b.b.Len() < 1<<20 {
		left := (1 << 20) - b.b.Len()
		if len(p) > left {
			p = p[:left]
		}
		_, _ = b.b.Write(p)
	}
	return n, nil
}
func (b *limitedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

var _ io.Writer = (*limitedBuffer)(nil)
