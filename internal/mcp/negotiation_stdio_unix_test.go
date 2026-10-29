//go:build darwin || linux

package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"dsgo/internal/state"
)

func TestNegotiationStdioServerHelper(t *testing.T) {
	mode := os.Getenv("DSGO_MCP_NEGOTIATION_MODE")
	if mode == "" {
		return
	}
	logPath := os.Getenv("DSGO_MCP_NEGOTIATION_LOG")
	pid := os.Getpid()
	record := func(value string) {
		file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err == nil {
			fmt.Fprintf(file, "%d\t%s\n", pid, value)
			file.Close()
		}
	}
	if previous, err := os.ReadFile(logPath); err == nil {
		for _, line := range strings.Split(string(previous), "\n") {
			fields := strings.Split(line, "\t")
			if len(fields) == 2 && fields[1] == "start" {
				prior, _ := strconv.Atoi(fields[0])
				if prior > 0 && prior != pid && syscall.Kill(prior, 0) == nil {
					record("overlap")
					os.Exit(91)
				}
			}
		}
	}
	record("start")
	var once sync.Once
	cleanup := func() { once.Do(func() { record("exit") }) }
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, syscall.SIGTERM)
	go func() { <-interrupts; cleanup(); os.Exit(0) }()
	encoder := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request wire
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			cleanup()
			os.Exit(92)
		}
		record("method\t" + request.Method)
		if request.Method == "" {
			record("unexpected-response")
			cleanup()
			os.Exit(93)
		}
		result := any(nil)
		switch request.Method {
		case "server/discover":
			if mode == "legacy-exit" {
				cleanup()
				os.Exit(0)
			}
			if mode == "legacy-silent" {
				continue
			}
			_ = encoder.Encode(wire{JSONRPC: "2.0", ID: json.RawMessage(`999`), Method: "ping"})
			result = modernDiscover(true)
		case "initialize":
			if mode == "modern" {
				cleanup()
				os.Exit(94)
			}
			var params map[string]json.RawMessage
			_ = json.Unmarshal(request.Params, &params)
			if _, ok := params["_meta"]; ok {
				cleanup()
				os.Exit(95)
			}
			result = map[string]any{"protocolVersion": LegacyProtocolVersion, "capabilities": map[string]any{"tools": map[string]bool{}}, "serverInfo": map[string]string{"name": "legacy", "version": "1"}}
		case "notifications/initialized", "notifications/cancelled":
			continue
		case "subscriptions/listen":
			if mode != "modern" {
				cleanup()
				os.Exit(96)
			}
			var id string
			_ = json.Unmarshal(request.ID, &id)
			_ = encoder.Encode(wire{JSONRPC: "2.0", Method: "notifications/subscriptions/acknowledged", Params: mustJSON(map[string]any{"_meta": map[string]any{subscriptionMetaKey: id}, "notifications": map[string]bool{"toolsListChanged": true}})})
			continue
		case "tools/list":
			result = map[string]any{"tools": []any{supervisorTool("echo")}}
			if mode == "modern" {
				result = cachedResult(result.(map[string]any))
			}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": "done"}}}
			if mode == "modern" {
				result = completeResult(result.(map[string]any))
			}
		default:
			_ = encoder.Encode(wire{JSONRPC: "2.0", ID: request.ID, Error: &RPCError{Code: -32601, Message: "unknown"}})
			continue
		}
		if mode == "modern" && request.Method != "server/discover" {
			var params map[string]json.RawMessage
			var meta map[string]json.RawMessage
			_ = json.Unmarshal(request.Params, &params)
			_ = json.Unmarshal(params["_meta"], &meta)
			if string(meta[protocolMetaKey]) != strconv.Quote(ProtocolVersion) {
				cleanup()
				os.Exit(97)
			}
		}
		_ = encoder.Encode(wire{JSONRPC: "2.0", ID: request.ID, Result: mustJSON(result)})
	}
	cleanup()
	os.Exit(0)
}

func TestStdioNegotiationUsesDisposableSiblingAndFreshServingPipe(t *testing.T) {
	for _, mode := range []string{"modern", "legacy-exit", "legacy-silent"} {
		t.Run(mode, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "processes.log")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			timeout := time.Second
			if mode == "legacy-silent" {
				timeout = 500 * time.Millisecond
			}
			client, err := OpenWithOptions(ctx, t.TempDir(), state.MCPServer{Command: os.Args[0], Args: []string{"-test.run=^TestNegotiationStdioServerHelper$"}, Env: map[string]string{"DSGO_MCP_NEGOTIATION_MODE": mode, "DSGO_MCP_NEGOTIATION_LOG": logPath}}, OpenOptions{ProbeTimeout: timeout})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if _, err := client.ListTools(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := client.CallTool(ctx, "echo", json.RawMessage(`{}`)); err != nil {
				t.Fatal(err)
			}
			if err := client.Close(); err != nil {
				t.Fatal(err)
			}
			log, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			starts := []string{}
			first := map[string]string{}
			for _, line := range strings.Split(string(log), "\n") {
				parts := strings.Split(line, "\t")
				if len(parts) == 2 && parts[1] == "start" {
					starts = append(starts, parts[0])
				}
				if len(parts) == 3 && parts[1] == "method" {
					if _, ok := first[parts[0]]; !ok {
						first[parts[0]] = parts[2]
					}
				}
			}
			if len(starts) != 2 || first[starts[0]] != "server/discover" || strings.Contains(string(log), "overlap") || strings.Contains(string(log), "unexpected-response") {
				t.Fatalf("probe isolation failed:\n%s", log)
			}
			expected := "initialize"
			if mode == "modern" {
				expected = "subscriptions/listen"
			}
			if first[starts[1]] != expected {
				t.Fatalf("serving child first message %q want %q:\n%s", first[starts[1]], expected, log)
			}
		})
	}
}
