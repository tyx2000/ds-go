//go:build darwin || linux

package tui

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestPagerRestoresRealTerminalOnQuitAndCancellation(t *testing.T) {
	if _, err := exec.LookPath("stty"); err != nil {
		t.Skip("stty unavailable")
	}
	for _, cancelRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "quit", true: "cancel"}[cancelRun], func(t *testing.T) {
			master, slave, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			defer slave.Close()
			if !IsTerminal(slave) {
				t.Fatal("PTY is not detected")
			}
			state := func() string {
				cmd := exec.Command("stty", "-g")
				cmd.Stdin = slave
				data, err := cmd.Output()
				if err != nil {
					t.Fatal(err)
				}
				return string(data)
			}
			before := state()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- Page(ctx, PagerOptions{Input: slave, Output: slave, Terminal: slave, Load: func() (Transcript, error) { return Project(fixtureEvents(), false), nil }})
			}()
			// Read the first complete paint, then drive real keyboard input through the PTY.
			var output bytes.Buffer
			painted := make(chan struct{})
			readDone := make(chan struct{})
			go func() {
				defer close(readDone)
				var buf [8192]byte
				seen := false
				for {
					n, err := master.Read(buf[:])
					if n > 0 {
						output.Write(buf[:n])
						if !seen && bytes.Contains(output.Bytes(), []byte("Trajectory")) {
							seen = true
							close(painted)
						}
					}
					if err != nil {
						return
					}
				}
			}()
			select {
			case <-painted:
			case <-time.After(3 * time.Second):
				t.Fatal("pager did not paint")
			}
			if cancelRun {
				cancel()
			} else {
				_, _ = master.Write([]byte("j\rtr\rq"))
			}
			select {
			case err = <-done:
				if cancelRun && err != context.Canceled {
					t.Fatalf("cancel result %v", err)
				}
				if !cancelRun && err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("pager failed to return")
			}
			after := state()
			if before != after {
				t.Fatalf("terminal state changed: before=%q after=%q", before, after)
			}
			slave.Close()
			master.Close()
			select {
			case <-readDone:
			case <-time.After(time.Second):
				t.Fatal("output reader stuck")
			}
			if !strings.Contains(output.String(), "\x1b[?1049h") || !strings.Contains(output.String(), "\x1b[?1049l") {
				t.Fatalf("alternate screen not restored: %q", output.String())
			}
			_, _ = io.Discard.Write(output.Bytes())
		})
	}
}
