//go:build darwin || linux

package tui

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dsgo/internal/tools"
	"github.com/creack/pty"
)

func TestTerminalPanelRealTTYRestoresOnDetachAndCancellation(t *testing.T) {
	if _, err := exec.LookPath("stty"); err != nil {
		t.Skip("stty unavailable")
	}
	for _, cancelRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "detach", true: "cancel"}[cancelRun], func(t *testing.T) {
			master, slave, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			defer slave.Close()
			if err = pty.Setsize(master, &pty.Winsize{Cols: 80, Rows: 24}); err != nil {
				t.Fatal(err)
			}
			before := terminalState(t, slave)
			controller, err := tools.NewTerminalController(tools.TerminalControllerOptions{Shell: &tools.TerminalShell{Path: "/bin/sh", Name: "sh", Args: []string{"-i"}}, ShellCandidates: []string{}, DisposeGraceMS: 100})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if err := controller.Close(ctx); err != nil {
					t.Error(err)
				}
			}()
			cwd := t.TempDir()
			_, err = controller.Create(context.Background(), "owner", cwd, tools.TerminalCreate{ID: "terminal", Cols: 80, Rows: 22})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			var preferenceCalls atomic.Int32
			go func() {
				done <- TerminalPanel(ctx, TerminalPanelOptions{Input: slave, Output: slave, Terminal: slave, Controller: controller, Owner: "owner", CWD: cwd, InitialID: "terminal", PreferredShell: func() string { preferenceCalls.Add(1); return "" }, RememberShell: func(string) { preferenceCalls.Add(1) }})
			}()
			var output bytes.Buffer
			var outputMu sync.Mutex
			painted := make(chan struct{})
			readDone := make(chan struct{})
			go func() {
				defer close(readDone)
				seen := false
				var data [8192]byte
				for {
					n, err := master.Read(data[:])
					if n > 0 {
						outputMu.Lock()
						if output.Len() < 1024*1024 {
							output.Write(data[:n])
						}
						if !seen && bytes.Contains(output.Bytes(), []byte("Ctrl+] return")) {
							seen = true
							close(painted)
						}
						outputMu.Unlock()
					}
					if err != nil {
						return
					}
				}
			}()
			select {
			case <-painted:
			case <-time.After(3 * time.Second):
				t.Fatal("panel did not paint")
			}
			if _, err = master.Write([]byte("printf '__%s__\\n' PANEL_READY\r")); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Second)
			seen := false
			for time.Now().Before(deadline) {
				outputMu.Lock()
				seen = bytes.Contains(output.Bytes(), []byte("__PANEL_READY__"))
				outputMu.Unlock()
				if seen {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if !seen {
				t.Fatal("keyboard input did not reach native PTY")
			}
			if err = pty.Setsize(master, &pty.Winsize{Cols: 40, Rows: 10}); err != nil {
				t.Fatal(err)
			}
			deadline = time.Now().Add(time.Second)
			resized := false
			for time.Now().Before(deadline) {
				rows := controller.List("owner")
				resized = len(rows) == 1 && rows[0].Cols == 40 && rows[0].Rows == 8
				if resized {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if !resized {
				t.Fatal("outer TTY resize did not reach same native PTY")
			}
			if cancelRun {
				cancel()
			} else {
				if _, err = master.Write([]byte{0x1d}); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err = <-done:
				if cancelRun && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				if !cancelRun && err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("panel failed to restore foreground terminal")
			}
			if after := terminalState(t, slave); after != before {
				t.Fatalf("terminal state changed %q -> %q", before, after)
			}
			if len(controller.List("owner")) != 1 {
				t.Fatal("detachment disposed user process")
			}
			if preferenceCalls.Load() != 0 {
				t.Fatal("recovering attachment consulted or changed new-shell preference")
			}
			slave.Close()
			master.Close()
			select {
			case <-readDone:
			case <-time.After(time.Second):
				t.Fatal("display reader leaked")
			}
		})
	}
}
