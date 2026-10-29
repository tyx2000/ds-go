package tools

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
	"unicode/utf8"
)

// TerminalSnapshot is an emulated screen, never untrusted PTY escape sequences.
// Each successful Next acknowledges all output through Sequence.
type TerminalSnapshot struct {
	Sequence             uint64
	Screen               string
	Info                 TerminalInfo
	Writable             bool
	CursorCol, CursorRow int
	CursorVisible        bool
	InputModes           []int
}

type TerminalAttachment struct {
	terminal      *userTerminal
	id            uint64
	ctx           context.Context
	cancel        context.CancelFunc
	stop          func() bool
	notified      chan struct{}
	dirty, closed bool
	pendingBytes  int
	err           error
	commands      sync.Mutex
	queuedBytes   int
}

type TerminalEnvironment struct {
	CWD              string `json:"cwd"`
	MaxInputBytes    int    `json:"maxInputBytes"`
	MaxBufferedBytes int    `json:"maxBufferedBytes"`
	MaxCols          int    `json:"maxCols"`
	MaxRows          int    `json:"maxRows"`
	Scrollback       int    `json:"scrollback"`
}

func (c *TerminalController) Environment(cwd string) TerminalEnvironment {
	return TerminalEnvironment{cwd, c.options.MaxInputBytes, c.options.MaxBufferedBytes, c.options.MaxCols, c.options.MaxRows, *c.options.Scrollback}
}

func (c *TerminalController) Attach(ctx context.Context, owner, id string) (*TerminalAttachment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	t, err := c.terminal(owner, id)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	if t.closing {
		t.mu.Unlock()
		return nil, terminalError("terminal/unavailable", "Terminal is closing")
	}
	t.nextAttachment++
	attachmentCtx, cancel := context.WithCancel(ctx)
	a := &TerminalAttachment{terminal: t, id: t.nextAttachment, ctx: attachmentCtx, cancel: cancel, notified: make(chan struct{}, 1), dirty: true}
	t.attachments[a.id] = a
	t.writer = a.id
	t.generation.Store(a.id)
	t.invalidateIdleLocked()
	t.changedLocked(0)
	t.mu.Unlock()
	a.stop = context.AfterFunc(attachmentCtx, func() { _ = a.Close() })
	return a, nil
}

func (a *TerminalAttachment) Next(ctx context.Context) (TerminalSnapshot, error) {
	for {
		t := a.terminal
		t.mu.Lock()
		if a.err != nil {
			err := a.err
			t.mu.Unlock()
			return TerminalSnapshot{}, err
		}
		if a.closed {
			t.mu.Unlock()
			return TerminalSnapshot{}, io.EOF
		}
		if a.dirty {
			a.dirty = false
			a.pendingBytes = 0
			col, row, visible := t.screen.Cursor()
			value := TerminalSnapshot{Sequence: t.sequence, Screen: t.screen.Render(), Info: t.infoLocked(), Writable: t.writableLocked(a), CursorCol: col, CursorRow: row, CursorVisible: visible, InputModes: t.screen.InputModes()}
			if len(value.Screen) > t.options.MaxBufferedBytes {
				a.failLocked(errors.New("terminal screen exceeds the configured follower buffer"))
				err := a.err
				t.mu.Unlock()
				return TerminalSnapshot{}, err
			}
			t.mu.Unlock()
			return value, nil
		}
		t.mu.Unlock()
		select {
		case <-ctx.Done():
			return TerminalSnapshot{}, ctx.Err()
		case <-a.ctx.Done():
			return TerminalSnapshot{}, a.ctx.Err()
		case <-a.notified:
		}
	}
}

// History returns a bounded, safe copy of retained rows. It does not acknowledge output.
func (a *TerminalAttachment) History(offset, limit int) ([]string, error) {
	t := a.terminal
	t.mu.Lock()
	defer t.mu.Unlock()
	if a.closed {
		return nil, io.EOF
	}
	if a.err != nil {
		return nil, a.err
	}
	if offset < 0 || limit < 0 {
		return nil, errors.New("invalid terminal history range")
	}
	return t.screen.Scrollback(offset, min(limit, t.info.Rows)), nil
}

func (a *TerminalAttachment) reserve(size int) error {
	t := a.terminal
	t.mu.Lock()
	defer t.mu.Unlock()
	if a.closed || a.err != nil {
		return terminalError("terminal/unavailable", "Terminal attachment is unavailable")
	}
	if size > t.options.MaxInputBytes {
		return errors.New("terminal input exceeds the configured limit")
	}
	if size+32 > t.options.MaxBufferedBytes-a.queuedBytes {
		return errors.New("terminal pending input exceeds the configured buffer")
	}
	a.queuedBytes += size + 32
	return nil
}
func (a *TerminalAttachment) release(size int) {
	t := a.terminal
	t.mu.Lock()
	a.queuedBytes -= size + 32
	t.mu.Unlock()
}
func (a *TerminalAttachment) Write(ctx context.Context, data []byte) error {
	if err := a.reserve(len(data)); err != nil {
		return err
	}
	defer a.release(len(data))
	a.commands.Lock()
	defer a.commands.Unlock()
	return a.execute(ctx, func(inputCtx context.Context, t *userTerminal) error {
		return t.process.Write(inputCtx, data, func() bool { return t.accepting.Load() && t.generation.Load() == a.id && a.ctx.Err() == nil })
	})
}
func (a *TerminalAttachment) Resize(ctx context.Context, cols, rows int) error {
	if err := a.reserve(0); err != nil {
		return err
	}
	defer a.release(0)
	a.commands.Lock()
	defer a.commands.Unlock()
	return a.execute(ctx, func(inputCtx context.Context, t *userTerminal) error {
		if cols < 2 || cols > t.options.MaxCols || rows < 1 || rows > t.options.MaxRows {
			return errors.New("terminal dimensions exceed configured limits")
		}
		if err := t.process.Resize(cols, rows); err != nil {
			return err
		}
		t.mu.Lock()
		t.screen.Resize(cols, rows)
		t.info.Cols, t.info.Rows = cols, rows
		t.changedLocked(0)
		t.mu.Unlock()
		return nil
	})
}
func (a *TerminalAttachment) execute(ctx context.Context, operation func(context.Context, *userTerminal) error) error {
	t := a.terminal
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-a.ctx.Done():
		return a.ctx.Err()
	case <-t.inputGate:
	}
	defer func() { t.inputGate <- struct{}{} }()
	t.mu.Lock()
	if !t.writableLocked(a) {
		reason := "read-only"
		if !t.accepting.Load() {
			reason = "not-running"
		}
		t.mu.Unlock()
		return terminalError("terminal/control-unavailable", reason)
	}
	t.invalidateIdleLocked()
	t.mu.Unlock()
	operationCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(a.ctx, cancel)
	defer func() { stop(); cancel() }()
	return operation(operationCtx, t)
}

func (a *TerminalAttachment) Close() error {
	t := a.terminal
	t.mu.Lock()
	if !a.closed {
		a.closed = true
		delete(t.attachments, a.id)
		if t.writer == a.id {
			t.writer = 0
			t.generation.Store(0)
		}
		t.invalidateIdleLocked()
		t.changedLocked(0)
		a.wakeLocked()
	}
	t.mu.Unlock()
	a.cancel()
	return nil
}
func (t *userTerminal) writableLocked(a *TerminalAttachment) bool {
	return !a.closed && a.err == nil && !t.closing && t.accepting.Load() && t.writer == a.id
}
func (a *TerminalAttachment) wakeLocked() {
	select {
	case a.notified <- struct{}{}:
	default:
	}
}
func (a *TerminalAttachment) failLocked(err error) {
	if a.err == nil {
		a.err = err
		a.dirty = false
		a.closed = true
		t := a.terminal
		delete(t.attachments, a.id)
		if t.writer == a.id {
			t.writer = 0
			t.generation.Store(0)
		}
		t.invalidateIdleLocked()
		for _, other := range t.attachments {
			other.dirty = true
			other.wakeLocked()
		}
		a.cancel()
		a.wakeLocked()
	}
}
func (t *userTerminal) changedLocked(bytes int) {
	for _, a := range t.attachments {
		if a.closed || a.err != nil {
			continue
		}
		a.pendingBytes += bytes
		if a.pendingBytes > t.options.MaxBufferedBytes {
			a.failLocked(errors.New("terminal follower is too slow; attach again for a fresh screen"))
			continue
		}
		a.dirty = true
		a.wakeLocked()
	}
}

func (t *userTerminal) readOutput() {
	defer close(t.readDone)
	var incomplete []byte
	var readError error
	for {
		var buffer [8192]byte
		n, err := t.process.Read(buffer[:])
		if n > 0 {
			data := append(incomplete, buffer[:n]...)
			end := 0
			for end < len(data) {
				if !utf8.FullRune(data[end:]) {
					break
				}
				_, size := utf8.DecodeRune(data[end:])
				end += size
			}
			incomplete = append([]byte{}, data[end:]...)
			if end > 0 {
				t.output([]byte(stringWithReplacement(data[:end])))
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readError = err
			}
			break
		}
	}
	if len(incomplete) > 0 {
		t.output([]byte("\ufffd"))
	}
	t.mu.Lock()
	if readError != nil && !t.closing {
		t.info.State = "failed"
		t.info.Error = readError.Error()
		t.accepting.Store(false)
	}
	t.mu.Unlock()
}
func stringWithReplacement(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	return string([]rune(string(data)))
}
func (t *userTerminal) output(data []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.screen.Write(data); err != nil {
		if !t.closing {
			t.info.State = "failed"
			t.info.Error = err.Error()
			t.accepting.Store(false)
		}
	}
	t.sequence++
	t.changedLocked(len(data))
}
func (t *userTerminal) writeReplies() {
	buffer := make([]byte, 4096)
	for {
		n, err := t.screen.Responses().Read(buffer)
		if n > 0 {
			// Web xterm replies travel through its writable attachment. Drain
			// queries even without a view, but do not invent unattended input.
			generation := t.generation.Load()
			if generation == 0 || !t.accepting.Load() {
				if err != nil {
					return
				}
				continue
			}
			select {
			case <-t.ctx.Done():
				return
			case <-t.inputGate:
			}
			_ = t.process.Write(t.ctx, buffer[:n], func() bool { return t.ctx.Err() == nil && t.accepting.Load() && t.generation.Load() == generation })
			t.inputGate <- struct{}{}
		}
		if err != nil {
			return
		}
	}
}
func (t *userTerminal) waitExit() {
	<-t.process.Exited()
	t.accepting.Store(false)
	// Output drainage may continue after the shell exits. Process-range cleanup
	// is performed only by explicit/retention disposal, never by detachment.
	<-t.readDone
	code, err := t.process.ExitResult()
	t.mu.Lock()
	if t.info.State != "failed" {
		if err != nil {
			t.info.State = "failed"
			t.info.Error = err.Error()
		} else {
			t.info.State = "exited"
			if code >= 0 {
				t.info.ExitCode = &code
			}
		}
	}
	t.changedLocked(0)
	t.mu.Unlock()
	close(t.drained)
}
func (t *userTerminal) close(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.closeGate:
	}
	defer func() { t.closeGate <- struct{}{} }()
	t.mu.Lock()
	t.closing = true
	t.accepting.Store(false)
	t.generation.Store(0)
	t.changedLocked(0)
	t.mu.Unlock()
	if err := t.process.Stop(ctx); err != nil {
		t.mu.Lock()
		t.retryNeeded = true
		t.signalMonitorLocked()
		t.mu.Unlock()
		return err
	}
	// Closing the response pipe also wakes an emulator query write, without
	// contending with the screen mutex while a protocol response is in flight.
	_ = t.screen.Close()
	t.cancel()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.drained:
	}
	t.mu.Lock()
	for _, a := range t.attachments {
		a.closed = true
		a.wakeLocked()
		a.cancel()
	}
	t.attachments = map[uint64]*TerminalAttachment{}
	t.mu.Unlock()
	return nil
}

func (t *userTerminal) invalidateIdleLocked() {
	t.activityEpoch++
	t.idleSince = time.Time{}
	t.signalMonitorLocked()
}
