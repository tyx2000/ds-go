//go:build darwin || linux

package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

type nativeUserTerminal struct {
	mu               sync.Mutex
	ioMu             sync.RWMutex
	stopMu           sync.Mutex
	master           *os.File
	fd               int
	cmd              *exec.Cmd
	exited           chan struct{}
	closed           chan struct{}
	closeOnce        sync.Once
	code             int
	err              error
	grace            time.Duration
	root             terminalProcessIdentity
	tracked          map[int]terminalProcessIdentity
	activity         *terminalShellActivity
	activityKey      string
	activityRevision uint64
	cleanup          func()
}
type terminalProcessIdentity struct {
	pid, parent, session int
	started, state       string
}

func startUserTerminal(ctx context.Context, owner, cwd string, shell TerminalShell, cols, rows int, grace time.Duration) (terminalProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cols > 65535 || rows > 65535 {
		return nil, errors.New("terminal dimensions exceed native PTY limits")
	}
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return nil, errors.New("terminal cwd is not a directory")
	}
	temporary, err := os.MkdirTemp("", "dsgo-user-terminal-")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(temporary) }
	env := filteredEnv(processEnvironment(os.Environ()), "TERM")
	env = append(env, "TMPDIR="+temporary, "TMP="+temporary, "TEMP="+temporary, "TERM=xterm-256color", "DSH_SESSION_ID="+owner)
	activity, args, env, err := prepareTerminalShellActivity(shell, env)
	if err != nil {
		cleanup()
		return nil, err
	}
	if activity != nil {
		previous := cleanup
		cleanup = func() { activity.dispose(); previous() }
	}
	master, slave, err := pty.Open()
	if err != nil {
		cleanup()
		return nil, err
	}
	defer slave.Close()
	fail := func(err error) (terminalProcess, error) { master.Close(); cleanup(); return nil, err }
	if err = pty.Setsize(master, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}); err != nil {
		return fail(err)
	}
	cmd := exec.Command(shell.Path, args...)
	cmd.Dir = resolved
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err = cmd.Start(); err != nil {
		return fail(err)
	}
	// Capture the unreaped child's birth identity before a waiter can release
	// its pid. All later adoption and signalling is fenced against PID reuse.
	observed, err := terminalProcessSnapshot(ctx)
	root, found := observed[cmd.Process.Pid]
	if err != nil || !found {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if err == nil {
			err = errors.New("terminal process identity was not observable")
		}
		return fail(err)
	}
	fd := int(master.Fd())
	if err = unix.SetNonblock(fd, true); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fail(err)
	}
	p := &nativeUserTerminal{master: master, fd: fd, cmd: cmd, exited: make(chan struct{}), closed: make(chan struct{}), grace: grace, root: root, tracked: map[int]terminalProcessIdentity{}, activity: activity, cleanup: cleanup}
	p.adoptLocked(observed)
	go func() {
		waitErr := cmd.Wait()
		code := 0
		if waitErr != nil {
			var exit *exec.ExitError
			if errors.As(waitErr, &exit) {
				code = exit.ExitCode()
			} else {
				p.mu.Lock()
				p.err = waitErr
				p.mu.Unlock()
			}
		}
		p.mu.Lock()
		p.code = code
		p.mu.Unlock()
		close(p.exited)
	}()
	if err = ctx.Err(); err != nil {
		return p, err
	}
	return p, nil
}
func (p *nativeUserTerminal) Read(buffer []byte) (int, error) {
	for {
		p.ioMu.RLock()
		n, err := unix.Read(p.fd, buffer)
		p.ioMu.RUnlock()
		if n > 0 {
			return n, nil
		}
		if err == nil {
			return 0, io.EOF
		}
		if errors.Is(err, syscall.EIO) || errors.Is(err, syscall.EBADF) {
			return 0, io.EOF
		}
		if !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			return 0, err
		}
		select {
		case <-p.closed:
			return 0, io.EOF
		case <-p.exited:
			return 0, io.EOF
		case <-time.After(10 * time.Millisecond):
		}
	}
}
func (p *nativeUserTerminal) Write(ctx context.Context, data []byte, allowed func() bool) error {
	if len(data) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !allowed() {
		return terminalError("terminal/control-unavailable", "read-only")
	}
	// Prompt evidence is invalidated by actual provider input, never by pane
	// metadata, attachment changes, window holds or grid resizing.
	p.InvalidateActivity()
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !allowed() {
			return terminalError("terminal/control-unavailable", "read-only")
		}
		select {
		case <-p.exited:
			return terminalError("terminal/control-unavailable", "not-running")
		case <-p.closed:
			return terminalError("terminal/control-unavailable", "not-running")
		default:
		}
		p.ioMu.RLock()
		n, err := unix.Write(p.fd, data)
		p.ioMu.RUnlock()
		if n > 0 {
			data = data[n:]
		}
		if err == nil {
			continue
		}
		if !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.closed:
			return io.ErrClosedPipe
		case <-time.After(10 * time.Millisecond):
		}
	}
	return nil
}
func (p *nativeUserTerminal) Resize(cols, rows int) error {
	if cols > 65535 || rows > 65535 {
		return errors.New("terminal dimensions exceed native PTY limits")
	}
	select {
	case <-p.exited:
		return terminalError("terminal/control-unavailable", "not-running")
	case <-p.closed:
		return io.ErrClosedPipe
	default:
	}
	p.ioMu.RLock()
	defer p.ioMu.RUnlock()
	return unix.IoctlSetWinsize(p.fd, unix.TIOCSWINSZ, &unix.Winsize{Col: uint16(cols), Row: uint16(rows)})
}
func (p *nativeUserTerminal) Exited() <-chan struct{} { return p.exited }
func (p *nativeUserTerminal) ExitResult() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code, p.err
}

func terminalIdentityAlive(identity terminalProcessIdentity, rows map[int]terminalProcessIdentity) bool {
	current, ok := rows[identity.pid]
	return ok && current.started == identity.started && !strings.HasPrefix(current.state, "Z") && !strings.HasPrefix(current.state, "X")
}
func (p *nativeUserTerminal) adoptLocked(rows map[int]terminalProcessIdentity) []terminalProcessIdentity {
	for pid, identity := range p.tracked {
		if !terminalIdentityAlive(identity, rows) {
			delete(p.tracked, pid)
		}
	}
	if current, ok := rows[p.root.pid]; ok && current.started == p.root.started {
		owned := map[int]bool{p.root.pid: true}
		changed := true
		for changed {
			changed = false
			for pid, row := range rows {
				if !owned[pid] && (owned[row.parent] || row.session == p.root.pid) {
					owned[pid] = true
					changed = true
				}
			}
		}
		for pid := range owned {
			if pid != p.root.pid {
				p.tracked[pid] = rows[pid]
			}
		}
	}
	result := make([]terminalProcessIdentity, 0, len(p.tracked)+1)
	for _, identity := range p.tracked {
		if terminalIdentityAlive(identity, rows) {
			result = append(result, identity)
		}
	}
	if terminalIdentityAlive(p.root, rows) {
		result = append(result, p.root)
	}
	return result
}
func (p *nativeUserTerminal) observe(ctx context.Context) (map[int]terminalProcessIdentity, []terminalProcessIdentity, error) {
	rows, err := terminalProcessSnapshot(ctx)
	if err != nil {
		return nil, nil, err
	}
	p.mu.Lock()
	members := p.adoptLocked(rows)
	p.mu.Unlock()
	return rows, members, nil
}
func (p *nativeUserTerminal) signal(ctx context.Context, members []terminalProcessIdentity, signal syscall.Signal) error {
	var failures []error
	for _, identity := range members {
		rows, err := terminalProcessSnapshot(ctx)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if !terminalIdentityAlive(identity, rows) {
			continue
		}
		if err = syscall.Kill(identity.pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
func (p *nativeUserTerminal) Stop(ctx context.Context) error {
	p.stopMu.Lock()
	defer p.stopMu.Unlock()
	select {
	case <-p.closed:
		return nil
	default:
	}
	_, members, err := p.observe(ctx)
	if err != nil {
		return err
	}
	if err = p.signal(ctx, members, syscall.SIGTERM); err != nil {
		return err
	}
	until := time.Now().Add(p.grace)
	for {
		_, members, err = p.observe(ctx)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			break
		}
		if !time.Now().Before(until) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(25*time.Millisecond, time.Until(until))):
		}
	}
	if len(members) > 0 {
		if err = p.signal(ctx, members, syscall.SIGKILL); err != nil {
			return err
		}
	}
	until = time.Now().Add(p.grace)
	for {
		_, members, err = p.observe(ctx)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			break
		}
		if !time.Now().Before(until) {
			return fmt.Errorf("terminal cleanup failed: %d surviving owned processes", len(members))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(25*time.Millisecond, time.Until(until))):
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.exited:
	}
	p.closeOnce.Do(func() { close(p.closed); p.ioMu.Lock(); _ = p.master.Close(); p.ioMu.Unlock(); p.cleanup() })
	return nil
}
func (p *nativeUserTerminal) InvalidateActivity() {
	p.mu.Lock()
	if p.activity != nil {
		p.activity.invalidate()
	}
	p.mu.Unlock()
}
func (p *nativeUserTerminal) InspectActivity(ctx context.Context) (string, uint64) {
	rows, members, err := p.observe(ctx)
	if err != nil {
		return "unknown", 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	state := "unknown"
	revision := uint64(0)
	select {
	case <-p.closed:
		state = "idle"
	default:
		if len(members) > 1 {
			state = "busy"
		} else if !terminalIdentityAlive(p.root, rows) {
			if len(members) == 0 {
				state = "idle"
			}
		} else if p.activity != nil {
			state, revision = p.activity.inspect(p.root.pid)
			p.ioMu.RLock()
			foreground, foregroundErr := unix.IoctlGetInt(p.fd, unix.TIOCGPGRP)
			p.ioMu.RUnlock()
			if foregroundErr != nil {
				state = "unknown"
			} else if foreground != p.root.pid {
				state = "busy"
			}
		}
	}
	key := state + ":" + strconv.FormatUint(revision, 10)
	if key != p.activityKey {
		p.activityRevision++
		p.activityKey = key
	}
	return state, p.activityRevision
}
