package tools

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeUserTerminal struct {
	output       chan []byte
	exited       chan struct{}
	once         sync.Once
	mu           sync.Mutex
	remaining    []byte
	input        []byte
	stopFailures int
	stopCalls    atomic.Int32
	state        string
	revision     uint64
	writeStarted chan struct{}
	writeRelease chan struct{}
}

func newFakeUserTerminal() *fakeUserTerminal {
	return &fakeUserTerminal{output: make(chan []byte, 16), exited: make(chan struct{}), state: "unknown", revision: 1}
}
func (f *fakeUserTerminal) Read(out []byte) (int, error) {
	if len(f.remaining) == 0 {
		next, ok := <-f.output
		if !ok {
			return 0, io.EOF
		}
		f.remaining = next
	}
	n := copy(out, f.remaining)
	f.remaining = f.remaining[n:]
	return n, nil
}
func (f *fakeUserTerminal) Write(ctx context.Context, data []byte, allowed func() bool) error {
	if f.writeStarted != nil {
		select {
		case f.writeStarted <- struct{}{}:
		default:
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.writeRelease:
		}
	}
	if !allowed() {
		return terminalError("terminal/control-unavailable", "read-only")
	}
	f.mu.Lock()
	f.input = append(f.input, data...)
	f.mu.Unlock()
	return nil
}
func (f *fakeUserTerminal) Resize(int, int) error { return nil }
func (f *fakeUserTerminal) finish()               { f.once.Do(func() { close(f.exited); close(f.output) }) }
func (f *fakeUserTerminal) Stop(ctx context.Context) error {
	f.stopCalls.Add(1)
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	fail := f.stopFailures > 0
	f.stopFailures--
	f.mu.Unlock()
	if fail {
		return errors.New("injected cleanup failure")
	}
	f.finish()
	return nil
}
func (f *fakeUserTerminal) Exited() <-chan struct{}  { return f.exited }
func (f *fakeUserTerminal) ExitResult() (int, error) { return 0, nil }
func (f *fakeUserTerminal) InspectActivity(context.Context) (string, uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, f.revision
}
func (f *fakeUserTerminal) InvalidateActivity() { f.mu.Lock(); f.revision++; f.mu.Unlock() }

func fakeTerminalController(t *testing.T, options TerminalControllerOptions) (*TerminalController, *fakeUserTerminal) {
	t.Helper()
	options.Shell = &TerminalShell{Path: "/bin/sh", Name: "sh", Args: []string{"-i"}}
	options.ShellCandidates = []string{}
	c, err := NewTerminalController(options)
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeUserTerminal()
	c.factory = func(context.Context, string, string, TerminalShell, int, int, time.Duration) (terminalProcess, error) {
		return f, nil
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := c.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return c, f
}
func createFakeTerminal(t *testing.T, c *TerminalController, owner, id string) {
	t.Helper()
	if _, err := c.Create(context.Background(), owner, "/tmp", TerminalCreate{ID: id, Cols: 40, Rows: 6}); err != nil {
		t.Fatal(err)
	}
}
func nextTerminalMatching(t *testing.T, a *TerminalAttachment, predicate func(TerminalSnapshot) bool) TerminalSnapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var last TerminalSnapshot
	for {
		snapshot, err := a.Next(ctx)
		if err != nil {
			t.Fatalf("%v; last info=%#v screen=%q", err, last.Info, last.Screen)
		}
		last = snapshot
		if predicate(snapshot) {
			return snapshot
		}
	}
}

func TestHostTerminalOwnerAndAttachmentControl(t *testing.T) {
	c, f := fakeTerminalController(t, TerminalControllerOptions{})
	createFakeTerminal(t, c, "owner", "term")
	if _, err := c.Attach(context.Background(), "other", "term"); err == nil {
		t.Fatal("cross-owner attachment allowed")
	}
	a, err := c.Attach(context.Background(), "owner", "term")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	first := nextTerminalMatching(t, a, func(s TerminalSnapshot) bool { return s.Writable })
	if first.Info.ID != "term" {
		t.Fatal(first)
	}
	b, err := c.Attach(context.Background(), "owner", "term")
	if err != nil {
		t.Fatal(err)
	}
	nextTerminalMatching(t, b, func(s TerminalSnapshot) bool { return s.Writable })
	nextTerminalMatching(t, a, func(s TerminalSnapshot) bool { return !s.Writable })
	if err = a.Write(context.Background(), []byte("old")); err == nil {
		t.Fatal("old follower retained input control")
	}
	if err = b.Write(context.Background(), []byte("new")); err != nil {
		t.Fatal(err)
	}
	if err = b.Resize(context.Background(), 50, 8); err != nil {
		t.Fatal(err)
	}
	if err = b.Close(); err != nil {
		t.Fatal(err)
	}
	if err = a.Write(context.Background(), []byte("replay")); err == nil {
		t.Fatal("older follower silently regained input")
	}
	f.mu.Lock()
	input := string(f.input)
	f.mu.Unlock()
	if input != "new" {
		t.Fatalf("unexpected input %q", input)
	}
	if len(c.List("owner")) != 1 {
		t.Fatal("detachment killed terminal")
	}
	if err = c.CloseTerminal(context.Background(), "owner", "term"); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Create(context.Background(), "owner", "/tmp", TerminalCreate{ID: "term", Cols: 40, Rows: 6}); err == nil {
		t.Fatal("closed identity reopened")
	}
}

func TestHostTerminalLatestAttachRevokesPendingInput(t *testing.T) {
	c, f := fakeTerminalController(t, TerminalControllerOptions{})
	f.writeStarted = make(chan struct{}, 1)
	f.writeRelease = make(chan struct{})
	createFakeTerminal(t, c, "owner", "term")
	a, err := c.Attach(context.Background(), "owner", "term")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	done := make(chan error, 1)
	go func() { done <- a.Write(context.Background(), []byte("never-replay")) }()
	select {
	case <-f.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("write not started")
	}
	b, err := c.Attach(context.Background(), "owner", "term")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	close(f.writeRelease)
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("superseded queued input reached PTY")
		}
	case <-time.After(time.Second):
		t.Fatal("pending input did not settle")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.input) != 0 {
		t.Fatalf("unexpected replay %q", f.input)
	}
}

func TestHostTerminalScreenReattachUTF8AndProtocolIsolation(t *testing.T) {
	c, f := fakeTerminalController(t, TerminalControllerOptions{})
	createFakeTerminal(t, c, "owner", "term")
	a, err := c.Attach(context.Background(), "owner", "term")
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.output <- []byte("hello \xe4\xb8")
	f.output <- []byte("\xad\x1b]52;c;copied\x07\x1b]0;secret-title\x07\x1b[31m RED\x1b[0m")
	s := nextTerminalMatching(t, a, func(s TerminalSnapshot) bool { return strings.Contains(s.Screen, "RED") })
	if !strings.Contains(s.Screen, "中") || strings.Contains(s.Screen, "copied") || strings.Contains(s.Screen, "secret-title") || strings.Contains(s.Screen, "\x1b]") {
		t.Fatalf("unsafe or broken screen %q", s.Screen)
	}
	if !strings.Contains(s.Screen, "\x1b[31m") {
		t.Fatalf("SGR styling lost: %q", s.Screen)
	}
	a.Close()
	b, err := c.Attach(context.Background(), "owner", "term")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	restored, err := b.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if restored.Sequence != s.Sequence || restored.Screen != s.Screen {
		t.Fatalf("snapshot did not restore screen: %#v %#v", s, restored)
	}
	f.output <- []byte("\x1b[?1049h\x1b[2J\x1b[Hfull screen\x1b[?1h\x1b[?2004h")
	full := nextTerminalMatching(t, b, func(s TerminalSnapshot) bool { return strings.Contains(s.Screen, "full screen") })
	if strings.Contains(full.Screen, "hello") {
		t.Fatalf("alternate screen did not isolate screen %q", full.Screen)
	}
	if len(full.InputModes) != 2 {
		t.Fatalf("input modes not retained: %v", full.InputModes)
	}
	f.output <- []byte("\x1b[?1049l")
	nextTerminalMatching(t, b, func(s TerminalSnapshot) bool { return strings.Contains(s.Screen, "hello") })
}

func TestHostTerminalSlowFollowerFailsAndFreshAttachRecovers(t *testing.T) {
	c, f := fakeTerminalController(t, TerminalControllerOptions{MaxBufferedBytes: 1024})
	createFakeTerminal(t, c, "owner", "term")
	a, err := c.Attach(context.Background(), "owner", "term")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = a.Next(context.Background())
	f.output <- []byte(strings.Repeat("x", 2048))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for {
		_, err = a.Next(ctx)
		if err != nil {
			break
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("slow consumer was not explicitly failed")
	}
	if err = a.Write(context.Background(), []byte("input")); err == nil {
		t.Fatal("failed consumer retained input")
	}
	b, err := c.Attach(context.Background(), "owner", "term")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err = b.Next(ctx); err != nil {
		t.Fatal("fresh screen recovery", err)
	}
}

func TestHostTerminalCreateReservationDeduplicatesAndCloseCancels(t *testing.T) {
	c, f := fakeTerminalController(t, TerminalControllerOptions{MaxTerminals: 1})
	started := make(chan struct{})
	release := make(chan struct{})
	var count atomic.Int32
	c.factory = func(ctx context.Context, _ string, _ string, _ TerminalShell, _ int, _ int, _ time.Duration) (terminalProcess, error) {
		count.Add(1)
		close(started)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return f, nil
		}
	}
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() {
		_, err := c.Create(context.Background(), "owner", "/tmp", TerminalCreate{ID: "term", Cols: 40, Rows: 6})
		first <- err
	}()
	<-started
	go func() {
		_, err := c.Create(context.Background(), "owner", "/tmp", TerminalCreate{ID: "term", Cols: 40, Rows: 6})
		second <- err
	}()
	if _, err := c.Create(context.Background(), "owner", "/tmp", TerminalCreate{ID: "other", Cols: 40, Rows: 6}); err == nil {
		t.Fatal("pending allocation did not consume quota")
	}
	if err := c.CloseTerminal(context.Background(), "owner", "term"); err != nil {
		t.Fatal(err)
	}
	if err := <-first; err == nil {
		t.Fatal("cancelled creation succeeded")
	}
	if err := <-second; err == nil {
		t.Fatal("closed pending identity returned success")
	}
	if count.Load() != 1 {
		t.Fatalf("allocated %d duplicate shells", count.Load())
	}
	if len(c.List("owner")) != 0 {
		t.Fatal("cancelled terminal was published")
	}
}

func TestHostTerminalPublishedPendingIdentityConsumesOneSlot(t *testing.T) {
	c, _ := fakeTerminalController(t, TerminalControllerOptions{MaxTerminals: 2})
	createFakeTerminal(t, c, "owner", "first")
	c.mu.Lock()
	owner := c.owners["owner"]
	first := owner.terminals["first"]
	ready := make(chan struct{})
	close(ready)
	owner.pending["first"] = &terminalAllocation{ready: ready, cancel: func() {}, terminal: first}
	c.mu.Unlock()
	second := newFakeUserTerminal()
	c.factory = func(context.Context, string, string, TerminalShell, int, int, time.Duration) (terminalProcess, error) {
		return second, nil
	}
	if _, err := c.Create(context.Background(), "owner", "/tmp", TerminalCreate{ID: "second", Cols: 40, Rows: 6}); err != nil {
		t.Fatal("published pending identity consumed two slots", err)
	}
	if _, err := c.Create(context.Background(), "owner", "/tmp", TerminalCreate{ID: "third", Cols: 40, Rows: 6}); err == nil {
		t.Fatal("two distinct retained identities did not consume both slots")
	}
}

type terminalWaitingContext struct {
	context.Context
	once    sync.Once
	waiting chan struct{}
}

func (c *terminalWaitingContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestHostTerminalPendingWaiterRechecksClosureAfterReady(t *testing.T) {
	for _, scope := range []string{"terminal", "owner", "host"} {
		t.Run(scope, func(t *testing.T) {
			c, _ := fakeTerminalController(t, TerminalControllerOptions{})
			createFakeTerminal(t, c, "owner", "term")
			ready, cancelled := make(chan struct{}), make(chan struct{})
			c.mu.Lock()
			owner := c.owners["owner"]
			terminal := owner.terminals["term"]
			delete(owner.terminals, "term")
			pending := &terminalAllocation{ready: ready, cancel: func() { close(cancelled) }}
			owner.pending["term"] = pending
			c.mu.Unlock()
			ctx := &terminalWaitingContext{Context: context.Background(), waiting: make(chan struct{})}
			created := make(chan error, 1)
			go func() {
				_, err := c.Create(ctx, "owner", "/tmp", TerminalCreate{ID: "term", Cols: 40, Rows: 6})
				created <- err
			}()
			select {
			case <-ctx.waiting:
			case <-time.After(time.Second):
				t.Fatal("duplicate did not await pending allocation")
			}
			closed := make(chan error, 1)
			go func() {
				switch scope {
				case "terminal":
					closed <- c.CloseTerminal(context.Background(), "owner", "term")
				case "owner":
					closed <- c.CloseOwner(context.Background(), "owner")
				case "host":
					closed <- c.Close(context.Background())
				}
			}()
			select {
			case <-cancelled:
			case <-time.After(time.Second):
				t.Fatal("close did not fence pending identity")
			}
			// The provider finishes after close has linearized. A former successful
			// allocation result must not authorize this outstanding duplicate call.
			c.mu.Lock()
			owner.terminals["term"] = terminal
			pending.terminal = terminal
			delete(owner.pending, "term")
			close(ready)
			c.mu.Unlock()
			select {
			case err := <-created:
				if err == nil {
					t.Fatal("closed pending allocation was acknowledged as successful")
				}
				var typed *TerminalError
				if !errors.As(err, &typed) || typed.Code != "terminal/unavailable" {
					t.Fatal("unexpected refusal", err)
				}
			case <-time.After(time.Second):
				t.Fatal("duplicate did not settle after ready")
			}
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("close did not settle after provider completion")
			}
		})
	}
}

func TestHostTerminalFailedCleanupRetainsQuotaAndCanRetry(t *testing.T) {
	c, f := fakeTerminalController(t, TerminalControllerOptions{MaxTerminals: 1})
	f.stopFailures = 1
	createFakeTerminal(t, c, "owner", "term")
	if err := c.CloseTerminal(context.Background(), "owner", "term"); err == nil {
		t.Fatal("cleanup failure was ignored")
	}
	if len(c.List("owner")) != 1 {
		t.Fatal("failed cleanup released the slot")
	}
	if _, err := c.Create(context.Background(), "owner", "/tmp", TerminalCreate{ID: "other", Cols: 40, Rows: 6}); err == nil {
		t.Fatal("failed cleanup released allocation quota")
	}
	if _, err := c.Attach(context.Background(), "owner", "term"); err == nil {
		t.Fatal("closing identity accepted attach")
	}
	if err := c.CloseTerminal(context.Background(), "owner", "term"); err != nil {
		t.Fatal(err)
	}
	if len(c.List("owner")) != 0 {
		t.Fatal("successful retry did not release terminal")
	}
	if f.stopCalls.Load() != 2 {
		t.Fatalf("unexpected cleanup count %d", f.stopCalls.Load())
	}
}

func TestHostTerminalCloseOwnerIsFinalWithinHostLifetime(t *testing.T) {
	c, _ := fakeTerminalController(t, TerminalControllerOptions{})
	createFakeTerminal(t, c, "owner", "term")
	if err := c.CloseOwner(context.Background(), "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Create(context.Background(), "owner", "/tmp", TerminalCreate{ID: "other", Cols: 40, Rows: 6}); err == nil {
		t.Fatal("disposed owner restarted a shell")
	}
}

func TestHostTerminalFailedHostClosePreservesProtocolPumpsForRetry(t *testing.T) {
	c, f := fakeTerminalController(t, TerminalControllerOptions{})
	f.stopFailures = 1
	createFakeTerminal(t, c, "owner", "term")
	c.mu.Lock()
	terminal := c.owners["owner"].terminals["term"]
	c.mu.Unlock()
	if err := c.Close(context.Background()); err == nil {
		t.Fatal("cleanup failure was discarded")
	}
	if len(c.List("owner")) != 1 {
		t.Fatal("Host close forgot the still-owned provider")
	}
	f.output <- []byte("\x1b[6n")
	progress := make(chan struct{})
	go func() {
		for {
			terminal.mu.Lock()
			sequence := terminal.sequence
			terminal.mu.Unlock()
			if sequence > 0 {
				close(progress)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	select {
	case <-progress:
	case <-time.After(time.Second):
		_ = terminal.screen.Close()
		t.Fatal("failed Host disposal abandoned the emulator response reader")
	}
	f.mu.Lock()
	input := string(f.input)
	f.mu.Unlock()
	if input != "" {
		t.Fatalf("closing terminal received invented protocol input %q", input)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatal("cleanup retry", err)
	}
}

type promptEvidenceTerminal struct {
	*fakeUserTerminal
	fences atomic.Int32
}

func (f *promptEvidenceTerminal) InvalidateActivity() {
	f.fences.Add(1)
	f.mu.Lock()
	f.state = "unknown"
	f.revision++
	f.mu.Unlock()
}
func (f *promptEvidenceTerminal) Write(ctx context.Context, data []byte, allowed func() bool) error {
	if len(data) > 0 && ctx.Err() == nil && allowed() {
		f.InvalidateActivity()
	}
	return f.fakeUserTerminal.Write(ctx, data, allowed)
}

func TestHostTerminalMetadataAndWindowReleasePreserveIdlePromptEvidence(t *testing.T) {
	timeout := int64(20)
	c, _ := fakeTerminalController(t, TerminalControllerOptions{UnattendedTimeoutMS: &timeout, ActivityPollIntervalMS: 3})
	f := &promptEvidenceTerminal{fakeUserTerminal: newFakeUserTerminal()}
	f.state = "idle"
	c.factory = func(context.Context, string, string, TerminalShell, int, int, time.Duration) (terminalProcess, error) {
		return f, nil
	}
	createFakeTerminal(t, c, "owner", "term")
	window, cancelWindow := context.WithCancel(context.Background())
	defer cancelWindow()
	release, err := c.Retain(window, "owner", "term")
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.Attach(context.Background(), "owner", "term")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Resize(context.Background(), 50, 8); err != nil {
		t.Fatal(err)
	}
	if err = c.Rename("owner", "term", "metadata only"); err != nil {
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	if f.fences.Load() != 0 {
		t.Fatal("metadata/window activity fenced the shell's existing idle prompt")
	}
	release()
	deadline := time.Now().Add(time.Second)
	for len(c.List("owner")) > 0 && time.Now().Before(deadline) {
		time.Sleep(3 * time.Millisecond)
	}
	if len(c.List("owner")) != 0 {
		t.Fatal("releasing the last window hold permanently protected an already idle shell")
	}
}

func TestHostTerminalWritesAndProtocolRepliesUseProviderPromptFence(t *testing.T) {
	c, _ := fakeTerminalController(t, TerminalControllerOptions{})
	f := &promptEvidenceTerminal{fakeUserTerminal: newFakeUserTerminal()}
	f.state = "idle"
	c.factory = func(context.Context, string, string, TerminalShell, int, int, time.Duration) (terminalProcess, error) {
		return f, nil
	}
	createFakeTerminal(t, c, "owner", "term")
	// With no renderer attached the headless parser drains, but never forwards
	// a DSR response as otherwise unrequested terminal input.
	f.output <- []byte("\x1b[6n")
	c.mu.Lock()
	terminal := c.owners["owner"].terminals["term"]
	c.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		terminal.mu.Lock()
		sequence := terminal.sequence
		terminal.mu.Unlock()
		if sequence > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if f.fences.Load() != 0 {
		t.Fatal("unattached screen generated extra input")
	}
	a, err := c.Attach(context.Background(), "owner", "term")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err = a.Write(context.Background(), []byte("human")); err != nil {
		t.Fatal(err)
	}
	if f.fences.Load() != 1 {
		t.Fatal("human input did not fence prior prompt")
	}
	f.output <- []byte("\x1b[6n")
	deadline = time.Now().Add(time.Second)
	for f.fences.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if f.fences.Load() != 2 {
		t.Fatal("forwarded protocol reply bypassed the provider's prompt fence")
	}
}

func TestHostTerminalWindowHoldAndConservativeIdleReclamation(t *testing.T) {
	for _, state := range []string{"unknown", "busy", "idle"} {
		t.Run(state, func(t *testing.T) {
			timeout := int64(20)
			c, f := fakeTerminalController(t, TerminalControllerOptions{UnattendedTimeoutMS: &timeout, ActivityPollIntervalMS: 3})
			f.mu.Lock()
			f.state = state
			f.mu.Unlock()
			createFakeTerminal(t, c, "owner", "term")
			window, cancelWindow := context.WithCancel(context.Background())
			defer cancelWindow()
			release, err := c.Retain(window, "owner", "term")
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(35 * time.Millisecond)
			if len(c.List("owner")) != 1 {
				t.Fatal("window hold did not protect collapsed terminal")
			}
			release()
			deadline := time.Now().Add(150 * time.Millisecond)
			for len(c.List("owner")) > 0 && time.Now().Before(deadline) {
				time.Sleep(3 * time.Millisecond)
			}
			retained := len(c.List("owner")) != 0
			if (state == "idle") == retained {
				t.Fatalf("state %s retained=%v", state, retained)
			}
		})
	}
}

func TestHostTerminalSafeScreenAllowsOnlyDisplayStyling(t *testing.T) {
	value := "\x1b[31mred\x1b[0m\x1b[2J\x1b]52;c;clipboard\x07\x1bPpayload\x1b\\\x1b[?1049h\u009btext\n"
	if got := safeTerminalRender(value); got != "\x1b[31mred\x1b[0mtext\n" {
		t.Fatalf("unsafe rendering %q", got)
	}
}
