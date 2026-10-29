//go:build darwin || linux

package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"dsgo/internal/state"
)

func TestStdioCloseServerHelper(t *testing.T) {
	mode := os.Getenv("DSGO_MCP_CLOSE_MODE")
	if mode == "" {
		return
	}
	path := os.Getenv("DSGO_MCP_CLOSE_LOG")
	record := func(line string) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err == nil {
			fmt.Fprintln(f, line)
			f.Close()
		}
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	go func() {
		<-signals
		record("term")
		if mode == "slow-term" {
			time.Sleep(400 * time.Millisecond)
			record("term-finished")
		}
		os.Exit(0)
	}()
	json.NewEncoder(os.Stdout).Encode(wire{JSONRPC: "2.0", Method: "notifications/ready"})
	io.Copy(io.Discard, os.Stdin)
	record("eof")
	if mode == "slow-eof" {
		time.Sleep(400 * time.Millisecond)
		record("eof-finished")
		os.Exit(0)
	}
	select {}
}

func TestStdioCloseKeepsSDKGraceAndProbeUsesTermFirst(t *testing.T) {
	for _, mode := range []string{"slow-eof", "slow-term", "probe"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "close.log")
			c, err := startTransport(t.TempDir(), state.MCPServer{Command: os.Args[0], Args: []string{"-test.run=^TestStdioCloseServerHelper$"}, Env: map[string]string{"DSGO_MCP_CLOSE_MODE": mode, "DSGO_MCP_CLOSE_LOG": path, "GORACE": "atexit_sleep_ms=0"}}, OpenOptions{}, false)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			select {
			case <-c.Notifications():
			case <-time.After(3 * time.Second):
				t.Fatal("server not ready")
			}
			c.probeTransport = mode == "probe"
			started := time.Now()
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			elapsed := time.Since(started)
			log, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "slow-eof":
				if !strings.Contains(string(log), "eof-finished") || strings.Contains(string(log), "term") {
					t.Fatalf("EOF cleanup killed early: %s", log)
				}
			case "slow-term":
				if !strings.Contains(string(log), "term-finished") || elapsed < 2*time.Second {
					t.Fatalf("TERM cleanup killed early (%s): %s", elapsed, log)
				}
			case "probe":
				if !strings.Contains(string(log), "term") || elapsed >= 1500*time.Millisecond {
					t.Fatalf("probe used normal EOF grace (%s): %s", elapsed, log)
				}
			}
		})
	}
}
