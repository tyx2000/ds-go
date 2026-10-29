package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dsgo/internal/core"
	"dsgo/internal/mcp"
	"dsgo/internal/state"
	"dsgo/internal/tools"
)

func mcpFixture(t *testing.T, label string, online *atomic.Bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(405)
			return
		}
		if r.Method == "DELETE" {
			w.WriteHeader(204)
			return
		}
		if online != nil && !online.Load() {
			w.WriteHeader(503)
			return
		}
		var request struct {
			ID     json.RawMessage
			Method string
			Params json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if len(request.ID) == 0 {
			w.WriteHeader(202)
			return
		}
		var result any
		switch request.Method {
		case "server/discover":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": -32601, "message": "legacy method"}})
			return
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fixture", "version": "1"}, "instructions": "Literal {" + label + "}"}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "echo", "description": label, "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": label}}}
		case "resources/list":
			result = map[string]any{"resources": []any{}}
		default:
			t.Errorf("unexpected fixture method %s", request.Method)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
}

func mcpTestApp(t *testing.T, cfg state.Config, watch bool) *App {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	a := &App{ctx: ctx, cancel: cancel, cfg: cfg, registry: tools.New(), out: io.Discard, err: &bytes.Buffer{}, flags: flags{cwd: t.TempDir()}}
	if watch {
		a.flags.home = t.TempDir()
		if err := state.SaveConfig(a.flags.home, cfg); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { a.closeMCP(); a.registry.Close(); cancel() })
	return a
}

func waitMCPCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("MCP condition did not settle")
}

func TestMCPInitialFailureDoesNotAbortAndRecoversInCLI(t *testing.T) {
	var online atomic.Bool
	srv := mcpFixture(t, "recovered", &online)
	defer srv.Close()
	initial, maximum, attempts := 10, 20, 10
	cfg := state.DefaultConfig()
	cfg.MCPServers["fixture"] = state.MCPServer{URL: srv.URL, Reconnect: &state.MCPReconnect{InitialDelayMS: &initial, MaxDelayMS: &maximum, MaxAttempts: &attempts}}
	a := mcpTestApp(t, cfg, false)
	if err := a.connectMCP(); err != nil {
		t.Fatal("optional MCP failure aborted startup", err)
	}
	if a.mcpSystemInstructions() != "" {
		t.Fatal("failed connection published instructions")
	}
	online.Store(true)
	waitMCPCondition(t, func() bool { return strings.Contains(a.mcpSystemInstructions(), "Literal {recovered}") })
	result, err := a.registry.Execute(a.ctx, core.ToolContext{Permission: "full-access"}, mcp.PublicToolName("fixture", "echo"), json.RawMessage(`{}`))
	if err != nil || result.Content != "recovered" {
		t.Fatalf("recovered dispatch: %+v %v", result, err)
	}
	if !strings.Contains(a.mcpSystemInstructions(), "### MCP server: fixture") {
		t.Fatal("instructions lack server attribution")
	}
}

func TestMCPConfigReloadReplacesConnectionAndClearsDisabledEntry(t *testing.T) {
	first := mcpFixture(t, "first", nil)
	defer first.Close()
	second := mcpFixture(t, "second", nil)
	defer second.Close()
	cfg := state.DefaultConfig()
	cfg.MCPServers["fixture"] = state.MCPServer{URL: first.URL}
	a := mcpTestApp(t, cfg, true)
	if err := a.connectMCP(); err != nil {
		t.Fatal(err)
	}
	before, err := a.mcpConnection("fixture")
	if err != nil {
		t.Fatal(err)
	}
	cfg.MCPServers["fixture"] = state.MCPServer{URL: second.URL}
	if err := state.SaveConfig(a.flags.home, cfg); err != nil {
		t.Fatal(err)
	}
	waitMCPCondition(t, func() bool { return strings.Contains(a.mcpSystemInstructions(), "Literal {second}") })
	if strings.Contains(a.mcpSystemInstructions(), "Literal {first}") {
		t.Fatal("old server instructions accumulated")
	}
	waitMCPCondition(t, func() bool { current, err := a.mcpConnection("fixture"); return err == nil && current != before })
	result, err := a.registry.Execute(a.ctx, core.ToolContext{Permission: "full-access"}, mcp.PublicToolName("fixture", "echo"), json.RawMessage(`{}`))
	if err != nil || result.Content != "second" {
		t.Fatalf("replacement dispatch: %+v %v", result, err)
	}
	cfg.MCPServers["fixture"] = state.MCPServer{URL: second.URL, Disabled: true}
	if err := state.SaveConfig(a.flags.home, cfg); err != nil {
		t.Fatal(err)
	}
	waitMCPCondition(t, func() bool { return a.mcpSystemInstructions() == "" })
	for _, spec := range a.registry.Specs() {
		if strings.HasPrefix(spec.Name, "mcp__") || spec.Name == "mcp_resources" {
			t.Fatalf("disabled server still exposes %s", spec.Name)
		}
	}
}

func TestMCPStrictStartupFailure(t *testing.T) {
	var online atomic.Bool
	srv := mcpFixture(t, "unused", &online)
	defer srv.Close()
	cfg := state.DefaultConfig()
	cfg.MCPServers["fixture"] = state.MCPServer{URL: srv.URL, FailOnStartupError: true}
	a := mcpTestApp(t, cfg, false)
	if err := a.connectMCP(); err == nil {
		t.Fatal("strict MCP startup error ignored")
	}
}
