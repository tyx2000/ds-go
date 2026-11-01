//go:build darwin || linux

package cli

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"dsgo/internal/core"
	"dsgo/internal/tui"
	"github.com/creack/pty"
)

func TestSCOR027PTYTakeoverRestoresCursorAndKeepsPrivateHistoryOut(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := pty.Setsize(master, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
		master.Close()
		slave.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	out := &takeoverOutput{changed: make(chan struct{}, 32)}
	reader := bufio.NewReader(slave)
	a := &App{ctx: ctx, in: reader, out: out}
	a.editor = tui.NewLineEditor(reader, slave, slave, func(text string) { _, _ = out.Write([]byte(text)) })
	t.Cleanup(func() { cancel(); master.Close(); slave.Close(); a.editor.Close() })
	type answer struct {
		text string
		err  error
	}
	composer := make(chan answer, 1)
	go func() {
		text, err := a.readInput(ctx, tui.LineOptions{Prompt: "compose> ", History: true}, true)
		composer <- answer{text, err}
	}()
	waitTakeoverOutput(t, out, "\x1b[6n", 1)
	if _, err := io.WriteString(master, "\x1b[1;10R草稿\x1b[D"); err != nil {
		t.Fatal(err)
	}
	waitTakeoverOutput(t, out, "草稿", 1)
	// A completed bracketed paste remains part of the suspended draft. The
	// approval reader must not inherit the composer's paste or history policy.
	if _, err := io.WriteString(master, "\x1b[200~界\x1b[201~"); err != nil {
		t.Fatal(err)
	}
	waitTakeoverOutput(t, out, "界", 1)
	approved := make(chan bool, 1)
	go func() {
		ok, err := a.approve(ctx, core.Approval{Description: "PTY private approval"})
		approved <- ok && err == nil
	}()
	waitTakeoverOutput(t, out, "\x1b[6n", 2)
	if _, err := io.WriteString(master, "\x1b[2;24Ryes\r"); err != nil {
		t.Fatal(err)
	}
	select {
	case ok := <-approved:
		if !ok {
			t.Fatal("private approval failed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("approval did not take over terminal input")
	}
	waitTakeoverOutput(t, out, "\x1b[6n", 3)
	// Up would select `yes` if private input leaked into editable history.
	// Otherwise the restored cursor is after 草界 and before 稿.
	if _, err := io.WriteString(master, "\x1b[3;10R\x1b[Ax\r"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-composer:
		if got.err != nil || got.text != "草界x稿" {
			t.Fatalf("private history or suspended draft/cursor was corrupted: %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("terminal composer did not resume")
	}
}

func TestSCOR027PTYBareEscapeDoesNotStopComposer(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	out := &takeoverOutput{changed: make(chan struct{}, 32)}
	reader := bufio.NewReader(slave)
	a := &App{ctx: ctx, in: reader, out: out}
	a.editor = tui.NewLineEditor(reader, slave, slave, func(text string) { _, _ = out.Write([]byte(text)) })
	t.Cleanup(func() { cancel(); master.Close(); slave.Close(); a.editor.Close() })
	type answer struct {
		text string
		err  error
	}
	done := make(chan answer, 1)
	go func() {
		text, err := a.readInput(ctx, tui.LineOptions{Prompt: "compose> ", History: true}, true)
		done <- answer{text, err}
	}()
	waitTakeoverOutput(t, out, "\x1b[6n", 1)
	if _, err := io.WriteString(master, "\x1b[1;10R\x1b"); err != nil {
		t.Fatal(err)
	}
	// The old escape timeout must not become a second timeout/EOF delivered
	// by the physical pump after the editor has returned to ordinary typing.
	time.Sleep(150 * time.Millisecond)
	if _, err := io.WriteString(master, "still alive\r"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.text != "still alive" {
			t.Fatalf("bare ESC killed subsequent typing: %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bare ESC left the logical input reader blocked")
	}
}

func TestSCOR027TrajectoryUsesSharedReaderAndReturnsToChat(t *testing.T) {
	for _, exitKey := range []string{"q", "\x03"} {
		t.Run(map[string]string{"q": "quit-key", "\x03": "control-c"}[exitKey], func(t *testing.T) {
			t.Setenv("TERM", "xterm-256color")
			t.Setenv("NO_COLOR", "")
			master, slave, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			if err := pty.Setsize(master, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
				master.Close()
				slave.Close()
				t.Fatal(err)
			}
			var modelCalls atomic.Int32
			a, session := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
				modelCalls.Add(1)
				return core.Response{Content: []core.Block{{Type: "text", Text: "unexpected public task"}}}, nil
			}))
			a.session = session
			reader := bufio.NewReader(slave)
			a.in, a.inputFile, a.out = reader, slave, slave
			out := &takeoverOutput{changed: make(chan struct{}, 32)}
			a.editor = tui.NewLineEditor(reader, slave, slave, func(text string) {
				a.writeMu.Lock()
				defer a.writeMu.Unlock()
				_, _ = io.WriteString(slave, text)
			})
			outputDone := make(chan struct{})
			masterFD := int(master.Fd())
			if err := syscall.SetNonblock(masterFD, true); err != nil {
				master.Close()
				slave.Close()
				t.Fatal(err)
			}
			go func() {
				defer close(outputDone)
				poll := time.NewTicker(5 * time.Millisecond)
				defer poll.Stop()
				var buffer [4096]byte
				for {
					if a.ctx.Err() != nil {
						return
					}
					n, err := syscall.Read(masterFD, buffer[:])
					if n > 0 {
						_, _ = out.Write(buffer[:n])
					}
					if err == syscall.EINTR {
						continue
					}
					if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
						select {
						case <-a.ctx.Done():
							return
						case <-poll.C:
						}
						continue
					}
					if err != nil || n == 0 {
						return
					}
				}
			}()
			chatDone := make(chan error, 1)
			go func() { chatDone <- a.chat() }()
			t.Cleanup(func() {
				a.cancel()
				select {
				case <-outputDone:
				case <-time.After(time.Second):
					t.Error("PTY output reader did not close")
				}
				master.Close()
				slave.Close()
				a.editor.Close()
			})
			waitTakeoverOutput(t, out, "\x1b[6n", 1)
			if _, err := io.WriteString(master, "\x1b[1;7R/trajectory\r"); err != nil {
				t.Fatal(err)
			}
			waitTakeoverOutput(t, out, "\x1b[?1049h", 1)
			waitTakeoverOutput(t, out, "Trajectory", 1)
			// In shared-reader mode a raw TTY idle period must not turn into a
			// permanent EOF in the pump, nor block the pager's refresh/cancel.
			time.Sleep(250 * time.Millisecond)
			if _, err := io.WriteString(master, exitKey); err != nil {
				t.Fatal(err)
			}
			waitTakeoverOutput(t, out, "\x1b[?1049l", 1)
			waitTakeoverOutput(t, out, "\x1b[6n", 2)
			if _, err := io.WriteString(master, "\x1b[2;7R/status\r"); err != nil {
				t.Fatal(err)
			}
			waitTakeoverOutput(t, out, `"events"`, 1)
			waitTakeoverOutput(t, out, "\x1b[6n", 3)
			if _, err := io.WriteString(master, "\x1b[3;7R/quit\r"); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-chatDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("chat did not resume after pager")
			}
			if modelCalls.Load() != 0 {
				t.Fatalf("pager exit key became public model input (%d calls)", modelCalls.Load())
			}
			events, err := a.store.Events(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if messages, ok, err := core.UserInputMessages(event); ok && err == nil {
					for _, message := range messages {
						if strings.Contains(string(core.JSON(message.Content)), "q/status") {
							t.Fatal("pager exit key contaminated the next composer")
						}
					}
				}
			}
		})
	}
}

func TestSCOR027PagerIdleRefreshAndCancellationLeaveInputUsable(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	out := &takeoverOutput{changed: make(chan struct{}, 32)}
	a := &App{ctx: ctx, in: bufio.NewReader(slave), inputFile: slave, out: out}
	t.Cleanup(func() { cancel(); master.Close(); slave.Close() })
	release, err := a.beginInputInteraction(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pageCtx, stopPage := context.WithCancel(ctx)
	defer stopPage()
	var loads atomic.Int32
	done := make(chan error, 1)
	go func() {
		reader := &pagerInput{logicalInput: logicalInput{ctx: pageCtx, stream: a.inputStream}}
		err := tui.Page(pageCtx, tui.PagerOptions{Input: slave, Reader: reader, Terminal: slave, Output: out, Load: func() (tui.Transcript, error) { loads.Add(1); return tui.Transcript{}, nil }})
		release()
		done <- err
	}()
	waitTakeoverOutput(t, out, "\x1b[?1049h", 1)
	// The second frame requires a 500ms idle refresh. A physical VMIN/VTIME
	// timeout incorrectly treated as EOF would prevent this from happening.
	waitTakeoverOutput(t, out, "Trajectory", 2)
	if loads.Load() < 2 {
		t.Fatal("pager did not refresh while no keys were pressed")
	}
	stopPage()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("pager cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("logical pager read ignored cancellation")
	}
	waitTakeoverOutput(t, out, "\x1b[?1049l", 1)
	composer := make(chan string, 1)
	go func() { line, _ := a.readInput(ctx, tui.LineOptions{Prompt: "compose> "}, true); composer <- line }()
	waitTakeoverOutput(t, out, "compose> ", 1)
	if _, err := io.WriteString(master, "after pager\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-composer:
		if line != "after pager" {
			t.Fatalf("cancelled pager contaminated composer: %q", line)
		}
	case <-time.After(time.Second):
		t.Fatal("pager left stdin stopped after cancellation")
	}
}
