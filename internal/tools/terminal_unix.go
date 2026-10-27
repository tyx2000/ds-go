//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"dsgo/internal/core"
	"github.com/creack/pty"
)

type terminalArgs struct {
	Action  string `json:"action"`
	ID      string `json:"id"`
	Command string `json:"command"`
	Input   string `json:"input"`
	Rows    int    `json:"rows"`
	Cols    int    `json:"cols"`
	Offset  int    `json:"offset"`
	Consume bool   `json:"consume"`
	WaitMS  int    `json:"wait_ms"`
	Sandbox string `json:"sandbox"`
}

func terminalSize(rows, cols int) (*pty.Winsize, error) {
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	if rows < 1 || rows > 1000 || cols < 1 || cols > 1000 {
		return nil, errors.New("terminal rows and cols must be 1..1000")
	}
	return &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}, nil
}

type terminalOutput struct{ j *job }

func (w terminalOutput) Write(b []byte) (int, error) {
	n, err := w.j.output.Write(b)
	select {
	case w.j.notify <- struct{}{}:
	default:
	}
	return n, err
}

func (r *Registry) terminalTool(ctx context.Context, tc core.ToolContext, input json.RawMessage) (core.ToolResult, error) {
	var a terminalArgs
	if err := decode(input, &a); err != nil {
		return core.ToolResult{}, err
	}
	if a.Offset < 0 {
		return core.ToolResult{}, errors.New("offset must be nonnegative")
	}
	if a.WaitMS < 0 || a.WaitMS > 30000 {
		return core.ToolResult{}, errors.New("wait_ms must be 0..30000")
	}
	if a.Action == "start" {
		return r.startTerminal(ctx, tc, input, a)
	}
	r.mu.RLock()
	if a.Action == "list" {
		jobs := []*job{}
		for _, j := range r.jobs {
			if j.owner == tc.SessionID && j.terminal != nil {
				jobs = append(jobs, j)
			}
		}
		r.mu.RUnlock()
		out := []any{}
		for _, j := range jobs {
			m := j.snapshot(0, false)
			delete(m, "output")
			out = append(out, m)
		}
		return result(out), nil
	}
	j := r.jobs[a.ID]
	r.mu.RUnlock()
	if j == nil || j.owner != tc.SessionID || j.terminal == nil {
		return core.ToolResult{}, errors.New("terminal does not exist in this session")
	}
	switch a.Action {
	case "read":
		if a.WaitMS > 0 {
			timer := time.NewTimer(time.Duration(a.WaitMS) * time.Millisecond)
			defer timer.Stop()
			select {
			case <-j.notify:
			case <-j.done:
			case <-timer.C:
			case <-ctx.Done():
				return core.ToolResult{}, ctx.Err()
			}
		}
		return result(j.snapshot(a.Offset, a.Consume)), nil
	case "write":
		if len(a.Input) > maxOutputBytes {
			return core.ToolResult{}, errors.New("terminal input exceeds 1 MiB limit")
		}
		select {
		case <-j.done:
			return core.ToolResult{}, errors.New("terminal has exited")
		default:
		}
		if err := ctx.Err(); err != nil {
			return core.ToolResult{}, err
		}
		n, err := j.terminal.Write([]byte(a.Input))
		if err != nil {
			return core.ToolResult{}, err
		}
		return result(map[string]any{"id": j.id, "written": n}), nil
	case "resize":
		sz, err := terminalSize(a.Rows, a.Cols)
		if err != nil {
			return core.ToolResult{}, err
		}
		if err := pty.Setsize(j.terminal, sz); err != nil {
			return core.ToolResult{}, err
		}
		return result(map[string]any{"id": j.id, "rows": sz.Rows, "cols": sz.Cols}), nil
	case "kill":
		j.cancel()
		select {
		case <-j.done:
			return result(j.snapshot(a.Offset, a.Consume)), nil
		case <-ctx.Done():
			return core.ToolResult{}, ctx.Err()
		}
	default:
		return core.ToolResult{}, errors.New("unknown terminal action")
	}
}

func (r *Registry) startTerminal(ctx context.Context, tc core.ToolContext, input json.RawMessage, a terminalArgs) (core.ToolResult, error) {
	if tc.SessionID == "" {
		return core.ToolResult{}, errors.New("terminal requires a session owner")
	}
	sz, err := terminalSize(a.Rows, a.Cols)
	if err != nil {
		return core.ToolResult{}, err
	}
	mode, err := requestedMode(ctx, tc, "terminal", input, a.Sandbox)
	if err != nil {
		return core.ToolResult{}, err
	}
	runctx, cancel := context.WithCancel(context.Background())
	args := []string{"-i"}
	if a.Command != "" {
		args = []string{"-c", a.Command}
	}
	sc, err := makeToolCommand(runctx, tc, mode, "/bin/sh", args...)
	if err != nil {
		cancel()
		return core.ToolResult{}, err
	}
	// PTY creates a new session/controlling terminal; Setpgid would conflict with setsid.
	sc.cmd.SysProcAttr.Setpgid = false
	if !strings.Contains(strings.Join(sc.cmd.Env, "\n"), "TERM=") {
		sc.cmd.Env = append(sc.cmd.Env, "TERM=xterm-256color")
	}
	f, err := pty.StartWithSize(sc.cmd, sz)
	if err != nil {
		cancel()
		sc.cleanup()
		return core.ToolResult{}, fmt.Errorf("PTY start failed: %w", err)
	}
	j := &job{id: newID(), owner: tc.SessionID, command: a.Command, cancel: cancel, done: make(chan struct{}), output: &boundedBuffer{limit: maxOutputBytes}, started: time.Now(), backend: sc.backend, mode: mode, pid: sc.cmd.Process.Pid, terminal: f, notify: make(chan struct{}, 1)}
	readDone := make(chan struct{})
	go func() { _, _ = io.Copy(terminalOutput{j}, f); close(readDone) }()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		cancel()
		_ = sc.cmd.Wait()
		f.Close()
		<-readDone
		sc.cleanup()
		return core.ToolResult{}, errors.New("registry closed")
	}
	r.jobs[j.id] = j
	r.mu.Unlock()
	go func() {
		err := sc.cmd.Wait()
		contextErr := runctx.Err()
		terminateProcess(sc.cmd)
		select {
		case <-readDone:
		case <-time.After(100 * time.Millisecond):
			f.Close()
			<-readDone
		}
		f.Close()
		sc.cleanup()
		cancel()
		j.mu.Lock()
		j.finished = time.Now()
		if err != nil {
			j.exitCode = -1
			j.err = err.Error()
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				j.exitCode = ee.ExitCode()
			}
		}
		if contextErr != nil && err != nil {
			j.err = contextErr.Error()
		}
		j.mu.Unlock()
		close(j.done)
	}()
	return result(j.snapshot(0, false)), nil
}
