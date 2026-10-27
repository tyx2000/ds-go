package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"dsgo/internal/core"
)

type boundedBuffer struct {
	mu         sync.Mutex
	b          []byte
	limit      int
	overflow   bool
	onOverflow func()
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	remaining := b.limit - len(b.b)
	if remaining > 0 {
		if remaining > n {
			remaining = n
		}
		b.b = append(b.b, p[:remaining]...)
	}
	if n > remaining && !b.overflow {
		b.overflow = true
		if b.onOverflow != nil {
			b.onOverflow()
		}
	}
	return n, nil
}
func (b *boundedBuffer) snapshot() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(bytes.Clone(b.b)), b.overflow
}

type sandboxCommand struct {
	cmd     *exec.Cmd
	cleanup func()
	backend string
	mode    string
}

func requestedMode(ctx context.Context, tc core.ToolContext, name string, input json.RawMessage, requested string) (string, error) {
	standing := tc.Permission
	if standing == "" {
		standing = "workspace-write"
	}
	if requested == "" {
		requested = standing
	}
	rank := map[string]int{"read-only": 0, "workspace-write": 1, "full-access": 2}
	s, ok := rank[standing]
	if !ok {
		return "", errors.New("unknown standing sandbox mode")
	}
	n, ok := rank[requested]
	if !ok {
		return "", errors.New("unsupported sandbox mode")
	}
	if n > s {
		if err := ask(ctx, tc, name, "Allow this one process with "+requested+" filesystem access?", input); err != nil {
			return "", err
		}
	}
	return requested, nil
}

func makeCommand(ctx context.Context, cwd, mode, executable string, args ...string) (sandboxCommand, error) {
	return makeCommandAt(ctx, cwd, cwd, mode, executable, args...)
}

// NewSandboxedCommand shares the built-in process policy with fixed runtimes.
// Call cleanup after the process and all of its output readers have finished.
func NewSandboxedCommand(ctx context.Context, cwd, mode, executable string, args ...string) (*exec.Cmd, func(), error) {
	return NewSandboxedCommandInWorkspace(ctx, cwd, cwd, mode, executable, args...)
}

func NewSandboxedCommandInWorkspace(ctx context.Context, cwd, workspace, mode, executable string, args ...string) (*exec.Cmd, func(), error) {
	sc, err := makeCommandAt(ctx, cwd, workspace, mode, executable, args...)
	if err != nil {
		return nil, nil, err
	}
	return sc.cmd, sc.cleanup, nil
}

func workspaceRoot(tc core.ToolContext) string {
	if tc.WorkspaceRoot != "" {
		return tc.WorkspaceRoot
	}
	return tc.CWD
}

func makeToolCommand(ctx context.Context, tc core.ToolContext, mode, executable string, args ...string) (sandboxCommand, error) {
	return makeCommandAt(ctx, tc.CWD, workspaceRoot(tc), mode, executable, args...)
}

func makeCommandAt(ctx context.Context, cwd, workspace, mode, executable string, args ...string) (sandboxCommand, error) {
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return sandboxCommand{}, err
	}
	st, err := os.Stat(resolved)
	if err != nil || !st.IsDir() {
		return sandboxCommand{}, errors.New("cwd is not a directory")
	}
	writable, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return sandboxCommand{}, err
	}
	rootInfo, err := os.Stat(writable)
	if err != nil || !rootInfo.IsDir() {
		return sandboxCommand{}, errors.New("workspace root is not a directory")
	}
	tmp, err := os.MkdirTemp("", "dsgo-process-")
	if err != nil {
		return sandboxCommand{}, err
	}
	tmp, err = filepath.EvalSymlinks(tmp)
	if err != nil {
		return sandboxCommand{}, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	backend := "none"
	command := executable
	argv := append([]string(nil), args...)
	if mode != "full-access" {
		switch runtime.GOOS {
		case "darwin":
			path, e := exec.LookPath("sandbox-exec")
			if e != nil {
				cleanup()
				return sandboxCommand{}, errors.New("restricted mode unavailable: macOS sandbox-exec was not found")
			}
			// This is filesystem enforcement. Network and process creation retain normal host access.
			profile := "(version 1)\n(allow default)\n(deny file-write*)\n"
			profile += "(allow file-write* (subpath " + seatbeltQuote(tmp) + "))\n"
			profile += "(allow file-write* (literal \"/dev/null\") (literal \"/dev/tty\") (regex #\"^/dev/fd/[0-9]+$\"))\n"
			if mode == "workspace-write" {
				profile += "(allow file-write* (subpath " + seatbeltQuote(writable) + "))\n"
			}
			argv = append([]string{"-p", profile, executable}, args...)
			command = path
			backend = "macos-seatbelt"
		case "linux":
			path, e := exec.LookPath("bwrap")
			if e != nil {
				cleanup()
				return sandboxCommand{}, errors.New("restricted mode unavailable: install bubblewrap, or deliberately select full-access")
			}
			argv = []string{"--die-with-parent", "--new-session", "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--bind", tmp, tmp, "--chdir", resolved}
			if mode == "workspace-write" {
				argv = append(argv, "--bind", writable, writable)
			}
			argv = append(argv, "--", executable)
			argv = append(argv, args...)
			command = path
			backend = "linux-bubblewrap"
		default:
			cleanup()
			return sandboxCommand{}, fmt.Errorf("restricted mode unavailable on %s; no weaker fallback is used", runtime.GOOS)
		}
	}
	cmd := exec.CommandContext(ctx, command, argv...)
	cmd.Dir = resolved
	cmd.Env = processEnvironment(os.Environ())
	cmd.Env = append(cmd.Env, "TMPDIR="+tmp, "TMP="+tmp, "TEMP="+tmp)
	configureProcess(cmd)
	cmd.WaitDelay = 2 * time.Second
	return sandboxCommand{cmd: cmd, cleanup: cleanup, backend: backend, mode: mode}, nil
}
func filteredEnv(env []string, names ...string) []string {
	out := []string{}
	for _, e := range env {
		keep := true
		for _, n := range names {
			if strings.HasPrefix(e, n+"=") {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, e)
		}
	}
	return out
}

// Credentials supplied to the harness are not inherited by model-written processes.
func processEnvironment(env []string) []string {
	clean := filteredEnv(env, "TMPDIR", "TMP", "TEMP")
	out := make([]string, 0, len(clean))
	for _, e := range clean {
		name, _, _ := strings.Cut(e, "=")
		upper := strings.ToUpper(name)
		sensitive := strings.HasPrefix(upper, "DSH_") || strings.HasPrefix(upper, "DSGO_")
		for _, part := range []string{"KEY", "PASSWORD", "SECRET", "TOKEN"} {
			if strings.Contains(upper, part) {
				sensitive = true
				break
			}
		}
		if !sensitive {
			out = append(out, e)
		}
	}
	return out
}

func execLookPath(path string) (string, error) {
	resolved, err := exec.LookPath(path)
	if err != nil {
		return "", fmt.Errorf("Node executable %q unavailable: %w", path, err)
	}
	return resolved, nil
}
func seatbeltQuote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

type shellArgs struct {
	Command    string `json:"command"`
	TimeoutMS  int    `json:"timeout_ms"`
	Background bool   `json:"background"`
	Sandbox    string `json:"sandbox"`
}

func deadline(ms int) (time.Duration, error) {
	if ms == 0 {
		ms = 120000
	}
	if ms < 1 || ms > 600000 {
		return 0, errors.New("timeout_ms must be 1..600000")
	}
	return time.Duration(ms) * time.Millisecond, nil
}

func (r *Registry) runShell(ctx context.Context, tc core.ToolContext, input json.RawMessage) (core.ToolResult, error) {
	var a shellArgs
	if err := decode(input, &a); err != nil {
		return core.ToolResult{}, err
	}
	if a.Command == "" {
		return core.ToolResult{}, errors.New("command must not be empty")
	}
	timeout, err := deadline(a.TimeoutMS)
	if err != nil {
		return core.ToolResult{}, err
	}
	mode, err := requestedMode(ctx, tc, "run_shell", input, a.Sandbox)
	if err != nil {
		return core.ToolResult{}, err
	}
	shell := "/bin/sh"
	if runtime.GOOS == "windows" {
		shell = "cmd.exe"
	}
	base := ctx
	if a.Background {
		base = context.Background()
	}
	runctx, cancel := context.WithTimeout(base, timeout)
	sc, err := makeToolCommand(runctx, tc, mode, shell, "-c", a.Command)
	if err != nil {
		cancel()
		return core.ToolResult{}, err
	}
	output := &boundedBuffer{limit: maxOutputBytes}
	sc.cmd.Stdout = output
	sc.cmd.Stderr = output
	if err := sc.cmd.Start(); err != nil {
		cancel()
		sc.cleanup()
		return core.ToolResult{}, fmt.Errorf("sandbox process start failed: %w", err)
	}
	j := &job{id: newID(), owner: tc.SessionID, command: a.Command, cancel: cancel, done: make(chan struct{}), output: output, started: time.Now(), backend: sc.backend, mode: mode, pid: sc.cmd.Process.Pid}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		cancel()
		_ = sc.cmd.Wait()
		sc.cleanup()
		return core.ToolResult{}, errors.New("registry closed")
	}
	r.jobs[j.id] = j
	r.mu.Unlock()
	go func() {
		err := sc.cmd.Wait()
		contextErr := runctx.Err()
		terminateProcess(sc.cmd)
		sc.cleanup()
		cancel()
		j.mu.Lock()
		j.finished = time.Now()
		j.exitCode = 0
		if err != nil {
			j.err = err.Error()
			j.exitCode = -1
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
	if a.Background {
		return result(j.snapshot(0, false)), nil
	}
	select {
	case <-j.done:
		return result(j.snapshot(0, false)), nil
	case <-ctx.Done():
		cancel()
		<-j.done
		return core.ToolResult{}, ctx.Err()
	}
}

type job struct {
	standard                          *standardBashState
	mu                                sync.Mutex
	id, owner, command, backend, mode string
	pid                               int
	cancel                            context.CancelFunc
	done                              chan struct{}
	output                            *boundedBuffer
	started, finished                 time.Time
	exitCode                          int
	err                               string
	cursor                            int
	terminal                          *os.File
	notify                            chan struct{}
	waiters                           int           // live waits a settlement releases instead of notifying
	killedByModel                     bool          // job_kill's result is the model's delivery
	noticed                           chan struct{} // closed once a completion notice is handled
}

func (j *job) snapshot(offset int, consume bool) map[string]any {
	if j.standard != nil {
		return j.standard.legacySnapshot(j, offset, consume)
	}
	return j.legacyView(offset, consume)
}

// legacyView is the jobs-tool projection over the combined write-order output.
func (j *job) legacyView(offset int, consume bool) map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	all, truncated := j.output.snapshot()
	if offset < 0 {
		offset = 0
	}
	if consume && offset == 0 {
		offset = j.cursor
	}
	if offset > len(all) {
		offset = len(all)
	}
	for offset < len(all) && all[offset]&0xc0 == 0x80 {
		offset++
	}
	if consume {
		j.cursor = len(all)
	}
	status := "running"
	if !j.finished.IsZero() {
		status = "finished"
	}
	m := map[string]any{"id": j.id, "command": j.command, "status": status, "pid": j.pid, "started_at": j.started, "output": all[offset:], "offset": offset, "next_offset": len(all), "output_truncated": truncated, "sandbox": map[string]any{"mode": j.mode, "backend": j.backend, "enforcement": "filesystem"}}
	if j.terminal != nil {
		m["terminal"] = true
	}
	if !j.finished.IsZero() {
		m["finished_at"] = j.finished
		m["exit_code"] = j.exitCode
		if j.err != "" {
			m["error"] = j.err
		}
	}
	return m
}

func (r *Registry) jobTool(ctx context.Context, tc core.ToolContext, input json.RawMessage) (core.ToolResult, error) {
	var a struct {
		Action    string `json:"action"`
		ID        string `json:"id"`
		Offset    int    `json:"offset"`
		Consume   bool   `json:"consume"`
		TimeoutMS int    `json:"timeout_ms"`
	}
	if err := decode(input, &a); err != nil {
		return core.ToolResult{}, err
	}
	if a.Offset < 0 {
		return core.ToolResult{}, errors.New("offset must be nonnegative")
	}
	r.mu.RLock()
	if a.Action == "list" {
		var jobs []*job
		for _, j := range r.jobs {
			if j.owner == tc.SessionID {
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
	j, ok := r.jobs[a.ID]
	r.mu.RUnlock()
	if !ok || j.owner != tc.SessionID {
		return core.ToolResult{}, errors.New("job does not exist in this session")
	}
	switch a.Action {
	case "output":
		return result(j.snapshot(a.Offset, a.Consume)), nil
	case "kill":
		j.requestKill("killed through jobs")
		select {
		case <-j.done:
			return result(j.snapshot(a.Offset, a.Consume)), nil
		case <-ctx.Done():
			return core.ToolResult{}, ctx.Err()
		}
	case "wait":
		wait := time.Second * 30
		if a.TimeoutMS != 0 {
			if a.TimeoutMS < 1 || a.TimeoutMS > 60000 {
				return core.ToolResult{}, errors.New("timeout_ms must be 1..60000")
			}
			wait = time.Duration(a.TimeoutMS) * time.Millisecond
		}
		if err := j.wait(ctx, wait); err != nil {
			return core.ToolResult{}, err
		}
		return result(j.snapshot(a.Offset, a.Consume)), nil
	default:
		return core.ToolResult{}, errors.New("unknown jobs action")
	}
}

var _ io.Writer = (*boundedBuffer)(nil)
