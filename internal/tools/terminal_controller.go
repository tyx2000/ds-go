package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
)

// TerminalController is a Host-only user interface capability. It is deliberately
// separate from Registry and the minimal preset's private, sandboxed Bash.
type TerminalController struct {
	mu       sync.Mutex
	options  TerminalControllerOptions
	owners   map[string]*terminalOwner
	lifetime context.Context
	cancel   context.CancelFunc
	closed   bool
	factory  func(context.Context, string, string, TerminalShell, int, int, time.Duration) (terminalProcess, error)
}

type TerminalShell struct {
	Path string   `json:"path"`
	Name string   `json:"name"`
	Args []string `json:"args"`
}
type TerminalControllerOptions struct {
	Shell                  *TerminalShell `json:"shell,omitempty"`
	ShellCandidates        []string       `json:"shellCandidates,omitempty"`
	MaxTerminals           int            `json:"maxTerminals,omitempty"`
	MaxCols                int            `json:"maxCols,omitempty"`
	MaxRows                int            `json:"maxRows,omitempty"`
	Scrollback             *int           `json:"scrollback,omitempty"`
	MaxBufferedBytes       int            `json:"maxBufferedBytes,omitempty"`
	MaxInputBytes          int            `json:"maxInputBytes,omitempty"`
	DisposeGraceMS         int64          `json:"disposeGraceMs,omitempty"`
	UnattendedTimeoutMS    *int64         `json:"unattendedTimeoutMs,omitempty"`
	ActivityPollIntervalMS int64          `json:"activityPollIntervalMs,omitempty"`
	CleanupRetryMS         int64          `json:"cleanupRetryMs,omitempty"`
}
type TerminalCreate struct {
	ID        string `json:"id"`
	ShellPath string `json:"shellPath,omitempty"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
}
type TerminalInfo struct {
	ID       string        `json:"id"`
	Shell    TerminalShell `json:"shell"`
	Title    string        `json:"title"`
	CWD      string        `json:"cwd"`
	Cols     int           `json:"cols"`
	Rows     int           `json:"rows"`
	State    string        `json:"state"`
	ExitCode *int          `json:"exitCode"`
	Error    string        `json:"error,omitempty"`
}
type TerminalError struct {
	Code    string
	Message string
}

func (e *TerminalError) Error() string         { return e.Message }
func terminalError(code, message string) error { return &TerminalError{code, message} }

type terminalOwner struct {
	ctx       context.Context
	cancel    context.CancelFunc
	closedIDs map[string]bool
	terminals map[string]*userTerminal
	pending   map[string]*terminalAllocation
	closing   bool
	order     []string
}
type terminalAllocation struct {
	ready    chan struct{}
	cancel   context.CancelFunc
	terminal *userTerminal
	err      error
}
type terminalProcess interface {
	Read([]byte) (int, error)
	Write(context.Context, []byte, func() bool) error
	Resize(int, int) error
	Stop(context.Context) error
	Exited() <-chan struct{}
	ExitResult() (int, error)
}

type terminalScreen interface {
	Write([]byte) error
	Resize(int, int)
	Render() string
	Cursor() (int, int, bool)
	InputModes() []int
	Scrollback(int, int) []string
	Responses() io.Reader
	Close() error
}
type userTerminal struct {
	mu                               sync.Mutex
	info                             TerminalInfo
	process                          terminalProcess
	screen                           terminalScreen
	options                          TerminalControllerOptions
	ctx                              context.Context
	ownerDone                        <-chan struct{}
	cancel                           context.CancelFunc
	inputGate                        chan struct{}
	readDone                         chan struct{}
	drained                          chan struct{}
	attachments                      map[uint64]*TerminalAttachment
	nextAttachment, writer, sequence uint64
	generation                       atomic.Uint64
	accepting                        atomic.Bool
	closeGate                        chan struct{}
	activityEpoch                    uint64
	idleSince, idleObserved          time.Time
	idleRevision                     uint64
	wakeup                           chan struct{}
	holders                          map[uint64]struct{}
	nextHolder                       uint64
	retryNeeded                      bool
	closing                          bool
}

func NewTerminalController(options TerminalControllerOptions) (*TerminalController, error) {
	if options.MaxTerminals == 0 {
		options.MaxTerminals = 8
	}
	if options.MaxCols == 0 {
		options.MaxCols = 500
	}
	if options.MaxRows == 0 {
		options.MaxRows = 200
	}
	if options.Scrollback == nil {
		n := 1000
		options.Scrollback = &n
	} else {
		n := *options.Scrollback
		options.Scrollback = &n
	}
	if options.MaxBufferedBytes == 0 {
		options.MaxBufferedBytes = 2 * 1024 * 1024
	}
	if options.MaxInputBytes == 0 {
		options.MaxInputBytes = 65536
	}
	if options.DisposeGraceMS == 0 {
		options.DisposeGraceMS = 1000
	}
	if options.UnattendedTimeoutMS == nil {
		n := int64(7200000)
		options.UnattendedTimeoutMS = &n
	} else {
		n := *options.UnattendedTimeoutMS
		options.UnattendedTimeoutMS = &n
	}
	if options.ActivityPollIntervalMS == 0 {
		options.ActivityPollIntervalMS = 30000
	}
	if options.CleanupRetryMS == 0 {
		options.CleanupRetryMS = 60000
	}
	if options.ShellCandidates == nil {
		options.ShellCandidates = []string{"zsh", "bash", "fish", "pwsh", "powershell", "cmd"}
	} else {
		options.ShellCandidates = append([]string{}, options.ShellCandidates...)
	}
	if options.MaxTerminals < 1 || options.MaxCols < 2 || options.MaxRows < 1 || *options.Scrollback < 0 || options.MaxBufferedBytes < 1024 || options.MaxInputBytes < 1 || options.DisposeGraceMS < 1 || *options.UnattendedTimeoutMS < 0 || options.ActivityPollIntervalMS < 1 || options.CleanupRetryMS < 1 {
		return nil, errors.New("invalid Host terminal limits")
	}
	for _, value := range []int64{options.DisposeGraceMS, *options.UnattendedTimeoutMS, options.ActivityPollIntervalMS, options.CleanupRetryMS} {
		if value > 9007199254740991 {
			return nil, errors.New("Host terminal timings must be safe integers")
		}
	}
	for _, name := range options.ShellCandidates {
		if name == "" {
			return nil, errors.New("terminal shell candidates must be nonempty")
		}
	}
	if options.Shell != nil {
		copy := *options.Shell
		copy.Args = append([]string{}, copy.Args...)
		options.Shell = &copy
		if copy.Path == "" || copy.Name == "" {
			return nil, errors.New("terminal shell profile requires path and name")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &TerminalController{options: options, owners: map[string]*terminalOwner{}, lifetime: ctx, cancel: cancel, factory: startUserTerminal}, nil
}

func NewHostTerminalID() string { return newID() }

func (c *TerminalController) Shells(ctx context.Context) ([]TerminalShell, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var defaultShell TerminalShell
	if c.options.Shell != nil {
		defaultShell = *c.options.Shell
		defaultShell.Args = append([]string{}, defaultShell.Args...)
	} else {
		path := os.Getenv("SHELL")
		if path == "" {
			path = "/bin/sh"
		}
		defaultShell = TerminalShell{Path: path, Name: filepath.Base(path), Args: automaticShellArgs(path)}
	}
	resolved, err := exec.LookPath(defaultShell.Path)
	if err != nil {
		return nil, errors.New("the execution environment's default shell is unavailable")
	}
	defaultShell.Path = resolved
	rows := []TerminalShell{defaultShell}
	seen := map[string]bool{shellIdentity(resolved): true}
	for _, name := range c.options.ShellCandidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path, err := exec.LookPath(name)
		if err != nil {
			if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		key := shellIdentity(path)
		if seen[key] {
			continue
		}
		seen[key] = true
		rows = append(rows, TerminalShell{Path: path, Name: filepath.Base(path), Args: automaticShellArgs(path)})
	}
	return rows, nil
}
func shellIdentity(path string) string {
	return strings.TrimSuffix(strings.ToLower(filepath.Base(path)), ".exe")
}
func automaticShellArgs(path string) []string {
	switch shellIdentity(path) {
	case "pwsh", "powershell":
		return []string{"-NoLogo"}
	case "cmd":
		return []string{}
	default:
		return []string{"-i"}
	}
}
func (c *TerminalController) List(owner string) []TerminalInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []TerminalInfo{}
	if state := c.owners[owner]; state != nil {
		for _, id := range state.order {
			if terminal := state.terminals[id]; terminal != nil {
				out = append(out, terminal.snapshotInfo())
			}
		}
	}
	return out
}
func (c *TerminalController) ownerLocked(owner string) *terminalOwner {
	state := c.owners[owner]
	if state == nil {
		ctx, cancel := context.WithCancel(c.lifetime)
		state = &terminalOwner{ctx: ctx, cancel: cancel, closedIDs: map[string]bool{}, terminals: map[string]*userTerminal{}, pending: map[string]*terminalAllocation{}}
		c.owners[owner] = state
	}
	return state
}

var hostTerminalID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (c *TerminalController) Create(ctx context.Context, owner, cwd string, request TerminalCreate) (TerminalInfo, error) {
	if owner == "" || !hostTerminalID.MatchString(request.ID) {
		return TerminalInfo{}, errors.New("invalid Host terminal identity")
	}
	if err := c.dimensions(request.Cols, request.Rows); err != nil {
		return TerminalInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return TerminalInfo{}, err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return TerminalInfo{}, terminalError("terminal/unavailable", "Terminal controller is closed")
	}
	state := c.ownerLocked(owner)
	if state.closing || state.closedIDs[request.ID] {
		c.mu.Unlock()
		return TerminalInfo{}, terminalError("terminal/unavailable", "Terminal was closed in this Session")
	}
	if existing := state.terminals[request.ID]; existing != nil {
		c.mu.Unlock()
		return existing.snapshotInfo(), nil
	}
	if pending := state.pending[request.ID]; pending != nil {
		c.mu.Unlock()
		return c.awaitAllocation(ctx, state, request.ID, pending)
	}
	// A published allocation can still have waiters in pending. Its identity
	// consumes exactly one slot while its completion is being acknowledged.
	used := len(state.terminals)
	for id := range state.pending {
		if _, published := state.terminals[id]; !published {
			used++
		}
	}
	if used >= c.options.MaxTerminals {
		c.mu.Unlock()
		return TerminalInfo{}, terminalError("terminal/limit-reached", "Session terminal limit reached")
	}
	allocationCtx, allocationCancel := context.WithCancel(ctx)
	stopOwner := context.AfterFunc(state.ctx, allocationCancel)
	pending := &terminalAllocation{ready: make(chan struct{}), cancel: allocationCancel}
	state.pending[request.ID] = pending
	c.mu.Unlock()
	terminal, err := c.allocate(allocationCtx, state.ctx, owner, cwd, request)
	stopOwner()
	allocationCancel()
	c.mu.Lock()
	if err == nil && (c.closed || state.closing || state.closedIDs[request.ID] || ctx.Err() != nil) {
		err = terminalError("terminal/unavailable", "Terminal creation was cancelled before publication")
	}
	if err == nil {
		state.terminals[request.ID] = terminal
		state.order = append(state.order, request.ID)
	}
	if err != nil && terminal != nil {
		state.closedIDs[request.ID] = true
		terminal.mu.Lock()
		terminal.info.State = "failed"
		terminal.info.Error = err.Error()
		terminal.mu.Unlock()
	}
	c.mu.Unlock()
	if err != nil && terminal != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cleanupErr := terminal.close(cleanupCtx)
		cancel()
		if cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
			c.mu.Lock()
			state.terminals[request.ID] = terminal
			state.order = append(state.order, request.ID)
			c.mu.Unlock()
		}
	}
	c.mu.Lock()
	delete(state.pending, request.ID)
	if err == nil && !c.openLocked(state, request.ID) {
		err = terminalError("terminal/unavailable", "Terminal was closed before creation acknowledgement")
	}
	pending.terminal, pending.err = terminal, err
	close(pending.ready)
	c.mu.Unlock()
	if terminal != nil {
		go c.monitor(owner, request.ID, terminal)
	}
	if terminal == nil {
		return TerminalInfo{}, err
	}
	return terminal.snapshotInfo(), err
}

func (c *TerminalController) openLocked(state *terminalOwner, id string) bool {
	return !c.closed && !state.closing && state.ctx.Err() == nil && !state.closedIDs[id]
}
func (c *TerminalController) awaitAllocation(ctx context.Context, state *terminalOwner, id string, pending *terminalAllocation) (TerminalInfo, error) {
	select {
	case <-ctx.Done():
		return TerminalInfo{}, ctx.Err()
	case <-pending.ready:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return TerminalInfo{}, err
	}
	if pending.err != nil {
		return TerminalInfo{}, pending.err
	}
	// Closure can win after allocation publication but before this waiter runs.
	// Recheck under the same lock that records closed identities and owners.
	if !c.openLocked(state, id) {
		return TerminalInfo{}, terminalError("terminal/unavailable", "Terminal was closed before creation acknowledgement")
	}
	if pending.terminal == nil {
		return TerminalInfo{}, errors.New("terminal allocation completed without a provider")
	}
	return pending.terminal.snapshotInfo(), nil
}

func (c *TerminalController) allocate(ctx, lifetime context.Context, owner, cwd string, request TerminalCreate) (*userTerminal, error) {
	shells, err := c.Shells(ctx)
	if err != nil {
		return nil, err
	}
	shell := shells[0]
	if request.ShellPath != "" {
		found := false
		for _, candidate := range shells {
			if candidate.Path == request.ShellPath {
				shell = candidate
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("selected shell is unavailable")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	process, err := c.factory(ctx, owner, cwd, shell, request.Cols, request.Rows, terminalDuration(c.options.DisposeGraceMS))
	if process == nil {
		return nil, err
	}
	// A failed disposal must leave both the provider and its readers available
	// for cleanup retry. Owner cancellation closes admission, not these pumps.
	terminalCtx, cancel := context.WithCancel(context.Background())
	t := &userTerminal{info: TerminalInfo{ID: request.ID, Shell: shell, Title: shell.Name, CWD: cwd, Cols: request.Cols, Rows: request.Rows, State: "running"}, process: process, options: c.options, ctx: terminalCtx, ownerDone: lifetime.Done(), cancel: cancel, inputGate: make(chan struct{}, 1), readDone: make(chan struct{}), drained: make(chan struct{}), closeGate: make(chan struct{}, 1), wakeup: make(chan struct{}, 1), holders: map[uint64]struct{}{}, attachments: map[uint64]*TerminalAttachment{}}
	t.screen = newUserTerminalScreen(request.Cols, request.Rows, *c.options.Scrollback)
	t.inputGate <- struct{}{}
	t.closeGate <- struct{}{}
	t.accepting.Store(true)
	go t.readOutput()
	go t.writeReplies()
	go t.waitExit()
	if err != nil {
		return t, err
	}
	if err = ctx.Err(); err != nil {
		return t, err
	}
	return t, nil
}

func (c *TerminalController) dimensions(cols, rows int) error {
	if cols < 2 || cols > c.options.MaxCols || rows < 1 || rows > c.options.MaxRows {
		return errors.New("terminal dimensions exceed configured limits")
	}
	return nil
}
func (t *userTerminal) snapshotInfo() TerminalInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.infoLocked()
}
func (t *userTerminal) infoLocked() TerminalInfo {
	copy := t.info
	copy.Shell.Args = append([]string{}, copy.Shell.Args...)
	if copy.ExitCode != nil {
		code := *copy.ExitCode
		copy.ExitCode = &code
	}
	return copy
}
func terminalDuration(ms int64) time.Duration {
	max := int64(^uint64(0)>>1) / int64(time.Millisecond)
	if ms > max {
		return time.Duration(int64(^uint64(0) >> 1))
	}
	return time.Duration(ms) * time.Millisecond
}

func (c *TerminalController) terminal(owner, id string) (*userTerminal, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.owners[owner]
	if c.closed || state == nil || state.closing || state.closedIDs[id] || state.terminals[id] == nil {
		return nil, terminalError("terminal/unavailable", "Terminal is unavailable in this Session")
	}
	return state.terminals[id], nil
}
func (c *TerminalController) Rename(owner, id, title string) error {
	if strings.TrimSpace(title) == "" || len(utf16.Encode([]rune(title))) > 120 {
		return errors.New("terminal title must contain 1..120 characters")
	}
	t, err := c.terminal(owner, id)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.info.Title = strings.TrimSpace(title)
	t.changedLocked(0)
	t.mu.Unlock()
	return nil
}
func (c *TerminalController) CloseTerminal(ctx context.Context, owner, id string) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	state := c.ownerLocked(owner)
	state.closedIDs[id] = true
	pending := state.pending[id]
	if pending != nil {
		pending.cancel()
	}
	c.mu.Unlock()
	if pending != nil {
		select {
		case <-pending.ready:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.mu.Lock()
	terminal := state.terminals[id]
	c.mu.Unlock()
	if terminal == nil {
		return nil
	}
	if err := terminal.close(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	delete(state.terminals, id)
	c.mu.Unlock()
	return nil
}
func (c *TerminalController) CloseOwner(ctx context.Context, owner string) error {
	c.mu.Lock()
	state := c.owners[owner]
	if state == nil {
		c.mu.Unlock()
		return nil
	}
	state.closing = true
	state.cancel()
	pending := []*terminalAllocation{}
	for _, allocation := range state.pending {
		allocation.cancel()
		pending = append(pending, allocation)
	}
	c.mu.Unlock()
	for _, allocation := range pending {
		select {
		case <-allocation.ready:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.mu.Lock()
	terminals := []*userTerminal{}
	for _, t := range state.terminals {
		terminals = append(terminals, t)
	}
	c.mu.Unlock()
	var failures []error
	for _, t := range terminals {
		if err := t.close(ctx); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	c.mu.Lock()
	state.terminals = map[string]*userTerminal{}
	c.mu.Unlock()
	return nil
}
func (c *TerminalController) Close(ctx context.Context) error {
	c.mu.Lock()
	c.closed = true
	c.cancel()
	owners := []string{}
	for owner := range c.owners {
		owners = append(owners, owner)
	}
	c.mu.Unlock()
	var failures []error
	for _, owner := range owners {
		if err := c.CloseOwner(ctx, owner); err != nil {
			failures = append(failures, fmt.Errorf("terminal owner cleanup: %w", err))
		}
	}
	return errors.Join(failures...)
}
