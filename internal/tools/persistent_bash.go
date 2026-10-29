package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"dsgo/internal/core"
)

const shellResetMessage = "The persistent bash shell was reset; the next bash call starts from the workspace with a fresh current directory and environment."
const shellClippedMessage = "<response clipped><NOTE>To save on context only part of this file has been shown to you. You should retry this tool after you have searched inside the file with `grep -n` in order to find the line numbers of what you are looking for.</NOTE>"

// bashEnvironment is owned by one session. A command deadline never leaves a
// half-executing shell available for the next call.
type bashEnvironment struct {
	gate            chan struct{}
	mode, workspace string
	ptySessionID    string
	cmd             *exec.Cmd
	pty             *os.File
	chunks          chan string
	done, readDone  chan struct{}
	cancel          context.CancelFunc
	cleanup         func()
	once            sync.Once
}

func (e *bashEnvironment) stop() {
	e.once.Do(func() {
		e.cancel()
		terminateProcess(e.cmd)
		_ = e.pty.Close()
		<-e.done
		<-e.readDone
		e.cleanup()
	})
}

func bashQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func newBashEnvironment(callCtx context.Context, tc core.ToolContext, ptyID string) (*bashEnvironment, error) {
	ctx, cancel := context.WithCancel(context.Background())
	sc, err := makeToolCommand(ctx, tc, tc.Permission, "/bin/bash", "--noprofile", "--norc", "-i")
	if err != nil {
		cancel()
		return nil, err
	}
	sc.cmd.Env = append(filteredEnv(sc.cmd.Env, "PS1", "PS2", "PROMPT_COMMAND"), "PS1=", "PS2=", "PROMPT_COMMAND=", "TERM=dumb")
	sc.cmd.Env = persistentShellEnvironment(sc.cmd.Env, tc.SessionID, ptyID)
	f, err := startBashPTY(sc.cmd)
	if err != nil {
		cancel()
		sc.cleanup()
		return nil, err
	}
	e := &bashEnvironment{gate: make(chan struct{}, 1), mode: tc.Permission, workspace: workspaceRoot(tc), ptySessionID: ptyID, cmd: sc.cmd, pty: f, chunks: make(chan string, 64), done: make(chan struct{}), readDone: make(chan struct{}), cancel: cancel, cleanup: sc.cleanup}
	go func() {
		defer close(e.readDone)
		buf := make([]byte, 8192)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				select {
				case e.chunks <- string(buf[:n]):
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() { _ = sc.cmd.Wait(); close(e.done) }()
	ready := "__DSGO_BASH_READY_" + newID() + "__"
	if _, err = f.Write([]byte("stty -echo; PS1=; PS2=; PROMPT_COMMAND=; printf '%s\\n' " + bashQuote(ready) + "\n")); err != nil {
		e.stop()
		return nil, err
	}
	initCtx, initCancel := context.WithTimeout(callCtx, 10*time.Second)
	defer initCancel()
	var tail string
	for {
		select {
		case s := <-e.chunks:
			tail += s
			// Echoed command source contains the nonce too: readiness requires
			// an isolated output line, not the source text echoed by the PTY.
			if strings.Contains("\n"+strings.ReplaceAll(tail, "\r\n", "\n"), "\n"+ready+"\n") {
				return e, nil
			}
			if len(tail) > 8192 {
				tail = tail[len(tail)-8192:]
			}
		case <-e.done:
			e.stop()
			return nil, errors.New("persistent bash exited during initialization")
		case <-initCtx.Done():
			e.stop()
			return nil, fmt.Errorf("persistent bash initialization: %w", initCtx.Err())
		}
	}
}

func (r *Registry) persistentBash(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	var a struct {
		Command string `json:"command"`
	}
	if err := decode(raw, &a); err != nil {
		return core.ToolResult{}, err
	}
	if strings.TrimSpace(a.Command) == "" {
		return core.ToolResult{}, errors.New("command must be a non-empty string")
	}
	if tc.SessionID == "" {
		return core.ToolResult{}, errors.New("bash requires an owning agent session")
	}
	if tc.Permission == "" {
		tc.Permission = "workspace-write"
	}
	if err := ctx.Err(); err != nil {
		return core.ToolResult{}, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return core.ToolResult{}, errors.New("registry closed")
	}
	e := r.shells[tc.SessionID]
	if e != nil && (e.mode != tc.Permission || e.workspace != workspaceRoot(tc)) {
		delete(r.shells, tc.SessionID)
		e.stop()
		e = nil
	}
	if e != nil {
		select {
		case <-e.done:
			delete(r.shells, tc.SessionID)
			e.stop()
			e = nil
		default:
		}
	}
	if e == nil {
		var err error
		r.nextManagedPTY++
		e, err = newBashEnvironment(ctx, tc, fmt.Sprintf("pty-%d", r.nextManagedPTY))
		if err != nil {
			r.mu.Unlock()
			return core.ToolResult{}, err
		}
		r.shells[tc.SessionID] = e
	}
	r.mu.Unlock()
	select {
	case e.gate <- struct{}{}:
	case <-ctx.Done():
		return core.ToolResult{}, ctx.Err()
	case <-e.done:
		return core.ToolResult{}, errors.New(shellResetMessage)
	}
	defer func() { <-e.gate }()
	if err := ctx.Err(); err != nil {
		return core.ToolResult{}, err
	}
	start, end := "__DSGO_BASH_START_"+newID()+"__", "__DSGO_BASH_END_"+newID()+":"
	wrapped := "printf '%s\\n' " + bashQuote(start) + "; eval -- " + bashQuote(a.Command) + "; __dsgo_bash_status=$?; printf '%s%s\\n' " + bashQuote(end) + " \"$__dsgo_bash_status\"\n"
	if _, err := io.WriteString(e.pty, wrapped); err != nil {
		e.stop()
		return core.ToolResult{}, err
	}
	runCtx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()
	var tail, prefix string
	started, clipped := false, false
	for {
		select {
		case s := <-e.chunks:
			tail += s
			if !started {
				if i := strings.Index(tail, start+"\r\n"); i >= 0 {
					tail = tail[i+len(start)+2:]
					started = true
				} else if i := strings.Index(tail, start+"\n"); i >= 0 {
					tail = tail[i+len(start)+1:]
					started = true
				} else {
					if len(tail) > 4096 {
						tail = tail[len(tail)-4096:]
					}
					continue
				}
			}
			if i := strings.Index(tail, end); i >= 0 {
				if j := strings.IndexByte(tail[i+len(end):], '\n'); j >= 0 {
					status := strings.TrimSpace(tail[i+len(end) : i+len(end)+j])
					if _, err := strconv.Atoi(status); err == nil {
						return core.ToolResult{Content: renderBashOutput(prefix+tail[:i], clipped) + "\n[Command finished with exit code " + status + "]"}, nil
					}
				}
			}
			if len(tail) > 8192 {
				n := len(tail) - 4096
				if len(prefix) < 65536 {
					remaining := 65536 - len(prefix)
					if n < remaining {
						remaining = n
					}
					prefix += tail[:remaining]
					if remaining < n {
						clipped = true
					}
				} else {
					clipped = true
				}
				tail = tail[n:]
			}
		case <-e.done:
			e.stop()
			return core.ToolResult{Content: renderBashOutput(prefix+tail, clipped) + "\n[shell exited]\n" + shellResetMessage}, nil
		case <-runCtx.Done():
			e.stop()
			if ctx.Err() != nil {
				return core.ToolResult{}, ctx.Err()
			}
			return core.ToolResult{Content: "Your command timed out after 300 seconds or experienced an OOM error. Below is partial output:\n" + renderBashOutput(prefix+tail, clipped) + "\n[Command timed out or OOM]\n" + shellResetMessage}, nil
		}
	}
}

func renderBashOutput(s string, clipped bool) string {
	s = strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	units := 0
	for i, r := range s {
		n := 1
		if r > 0xffff {
			n = 2
		}
		if units+n > 16000 {
			s = s[:i]
			clipped = true
			break
		}
		units += n
	}
	if clipped {
		s += shellClippedMessage
	}
	return s
}
