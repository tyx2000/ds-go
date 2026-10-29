package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dsgo/internal/state"
)

func supervisorPolicy() *ReconnectPolicy {
	return &ReconnectPolicy{InitialDelay: 2 * time.Millisecond, MaxDelay: 20 * time.Millisecond, MaxAttempts: 2}
}

func closeTestTransport(s *Supervisor) {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client != nil {
		client.fail(errors.New("fixture transport closed"))
	}
}

func waitSupervisor(t *testing.T, s *Supervisor, predicate func(SupervisorSnapshot) bool) SupervisorSnapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := s.Snapshot()
		if predicate(snapshot) {
			return snapshot
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("supervisor did not reach expected state: %+v", s.Snapshot())
	return SupervisorSnapshot{}
}

func mcpHTTPReply(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(result)
	_ = json.NewEncoder(w).Encode(wire{JSONRPC: "2.0", ID: id, Result: b})
}

func legacyProbeReply(w http.ResponseWriter, request wire) bool {
	if request.Method != "server/discover" {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(wire{JSONRPC: "2.0", ID: request.ID, Error: &RPCError{Code: -32601, Message: "legacy server"}})
	return true
}

func supervisorInitialize(w http.ResponseWriter, req wire, generation int64, instructions string) {
	w.Header().Set("Mcp-Session-Id", fmt.Sprint(generation))
	mcpHTTPReply(w, req.ID, map[string]any{"protocolVersion": LegacyProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{"listChanged": true}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}, "instructions": instructions})
}

func supervisorTool(name string) map[string]any {
	return map[string]any{"name": name, "inputSchema": map[string]any{"type": "object"}}
}

func TestSupervisorStartupRecoveryDoesNotReplayTools(t *testing.T) {
	var initializes, calls atomic.Int64
	var failCall atomic.Bool
	failCall.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(405)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(204)
			return
		}
		var req wire
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(400)
			return
		}
		if legacyProbeReply(w, req) {
			return
		}
		switch req.Method {
		case "initialize":
			n := initializes.Add(1)
			if n == 1 {
				w.WriteHeader(503)
				return
			}
			supervisorInitialize(w, req, n, "literal {server}\n")
		case "notifications/initialized":
			w.WriteHeader(202)
		case "tools/list":
			mcpHTTPReply(w, req.ID, map[string]any{"tools": []any{supervisorTool("mutate")}})
		case "tools/call":
			calls.Add(1)
			if failCall.Swap(false) {
				w.WriteHeader(404)
				return
			}
			mcpHTTPReply(w, req.ID, map[string]any{"content": []any{map[string]string{"type": "text", "text": "done"}}})
		}
	}))
	defer server.Close()
	s, err := OpenSupervisor(context.Background(), SupervisorOptions{Name: "server", CWD: t.TempDir(), Server: state.MCPServer{URL: server.URL}, Reconnect: supervisorPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first := waitSupervisor(t, s, func(v SupervisorSnapshot) bool { return v.Status == "connected" })
	if first.Instructions != "### MCP server: server\n\nliteral {server}" {
		t.Fatalf("instructions: %q", first.Instructions)
	}
	if _, err := s.CallTool(context.Background(), "mutate", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expired call succeeded")
	}
	if v := s.Snapshot(); v.Status != "connected" {
		t.Fatalf("one HTTP error silently changed generation: %+v", v)
	}
	closeTestTransport(s)
	waitSupervisor(t, s, func(v SupervisorSnapshot) bool { return v.Status == "connected" && v.Generation > first.Generation })
	if calls.Load() != 1 {
		t.Fatalf("side-effecting request replayed %d times", calls.Load())
	}
	if _, err := s.CallTool(context.Background(), "mutate", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("explicit second call missing")
	}
}

func TestSupervisorExhaustionAndDisabledRecovery(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprint(disabled), func(t *testing.T) {
			var initializes atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.WriteHeader(405)
					return
				}
				if r.Method == http.MethodDelete {
					w.WriteHeader(204)
					return
				}
				var req wire
				if json.NewDecoder(r.Body).Decode(&req) != nil {
					w.WriteHeader(400)
					return
				}
				if legacyProbeReply(w, req) {
					return
				}
				switch req.Method {
				case "initialize":
					n := initializes.Add(1)
					if n > 1 {
						w.WriteHeader(503)
						return
					}
					supervisorInitialize(w, req, n, "retained while disconnected")
				case "notifications/initialized":
					w.WriteHeader(202)
				case "tools/list":
					mcpHTTPReply(w, req.ID, map[string]any{"tools": []any{supervisorTool("tool")}})
				case "tools/call":
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			policy := supervisorPolicy()
			if disabled {
				no := false
				policy.Enabled = &no
			}
			var mu sync.Mutex
			sawRetained := false
			s, err := OpenSupervisor(context.Background(), SupervisorOptions{Name: "server", CWD: t.TempDir(), Server: state.MCPServer{URL: server.URL}, Reconnect: policy, Publish: func(v SupervisorSnapshot) error {
				mu.Lock()
				defer mu.Unlock()
				if v.Status == "disconnected" && len(v.Tools) == 1 && v.Instructions != "" {
					sawRetained = true
				}
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			_, _ = s.CallTool(context.Background(), "tool", json.RawMessage(`{}`))
			closeTestTransport(s)
			if disabled {
				v := waitSupervisor(t, s, func(v SupervisorSnapshot) bool { return v.Status == "disconnected" })
				if len(v.Tools) != 1 || v.Instructions == "" || v.Attempts != 0 {
					t.Fatalf("disabled state: %+v", v)
				}
			} else {
				v := waitSupervisor(t, s, func(v SupervisorSnapshot) bool { return v.Status == "exhausted" })
				if len(v.Tools) != 0 || v.Instructions != "" || v.Attempts != 2 || initializes.Load() != 3 {
					t.Fatalf("exhausted state: %+v, starts %d", v, initializes.Load())
				}
			}
			mu.Lock()
			retained := sawRetained
			mu.Unlock()
			if !retained {
				t.Fatal("last known tools removed before exhaustion")
			}
		})
	}
}

func TestSupervisorListChangedRefreshPreservesFailedDiscovery(t *testing.T) {
	var phase atomic.Int64
	phase.Store(1)
	notices := make(chan struct{}, 4)
	var listCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(204)
			return
		}
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			for {
				select {
				case <-r.Context().Done():
					return
				case <-notices:
					fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/list_changed\"}\n\n")
					w.(http.Flusher).Flush()
				}
			}
		}
		var req wire
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(400)
			return
		}
		if legacyProbeReply(w, req) {
			return
		}
		switch req.Method {
		case "initialize":
			supervisorInitialize(w, req, 1, "")
		case "notifications/initialized":
			w.WriteHeader(202)
		case "tools/list":
			listCalls.Add(1)
			tools := []any{supervisorTool("old")}
			if phase.Load() == 2 {
				tools = append(tools, supervisorTool("old"))
			}
			if phase.Load() == 3 {
				tools = []any{supervisorTool("new")}
			}
			if phase.Load() == 4 {
				tools = []any{supervisorTool("conflict")}
			}
			mcpHTTPReply(w, req.ID, map[string]any{"tools": tools})
		}
	}))
	defer server.Close()
	s, err := OpenSupervisor(context.Background(), SupervisorOptions{Name: "server", CWD: t.TempDir(), Server: state.MCPServer{URL: server.URL}, Publish: func(v SupervisorSnapshot) error {
		if len(v.Tools) > 0 && v.Tools[0].Name == "conflict" {
			return errors.New("foreign namespace conflict")
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	phase.Store(2)
	notices <- struct{}{}
	deadline := time.Now().Add(time.Second)
	for listCalls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if v := s.Snapshot(); len(v.Tools) != 1 || v.Tools[0].Name != "old" {
		t.Fatalf("failed discovery removed generation: %+v", v)
	}
	phase.Store(3)
	notices <- struct{}{}
	waitSupervisor(t, s, func(v SupervisorSnapshot) bool { return len(v.Tools) == 1 && v.Tools[0].Name == "new" })
	phase.Store(4)
	notices <- struct{}{}
	waitSupervisor(t, s, func(v SupervisorSnapshot) bool { return v.Status == "connected" && len(v.Tools) == 0 })
	phase.Store(3)
	notices <- struct{}{}
	waitSupervisor(t, s, func(v SupervisorSnapshot) bool { return len(v.Tools) == 1 && v.Tools[0].Name == "new" })
}

func TestSupervisorStrictStartupAndAttributedInstructionLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(405)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(204)
			return
		}
		var req wire
		_ = json.NewDecoder(r.Body).Decode(&req)
		if legacyProbeReply(w, req) {
			return
		}
		if req.Method == "initialize" {
			supervisorInitialize(w, req, 1, "abc")
			return
		}
		w.WriteHeader(202)
	}))
	defer server.Close()
	s, err := OpenSupervisor(context.Background(), SupervisorOptions{Name: "server", CWD: t.TempDir(), Server: state.MCPServer{URL: server.URL}, MaxInstructionBytes: 3, FailOnStartupError: true})
	if err == nil || s != nil || !strings.Contains(err.Error(), "maxInstructionBytes") {
		t.Fatalf("attribution limit not enforced: %v %v", s, err)
	}
}

func TestSupervisorCrashLoopSharesBudgetAndStableConnectionResetsIt(t *testing.T) {
	for _, stable := range []bool{false, true} {
		t.Run(fmt.Sprint(stable), func(t *testing.T) {
			var initializes atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.WriteHeader(405)
					return
				}
				if r.Method == http.MethodDelete {
					w.WriteHeader(204)
					return
				}
				var req wire
				_ = json.NewDecoder(r.Body).Decode(&req)
				if legacyProbeReply(w, req) {
					return
				}
				switch req.Method {
				case "initialize":
					n := initializes.Add(1)
					if n == 1 {
						w.WriteHeader(503)
						return
					}
					supervisorInitialize(w, req, n, "")
				case "notifications/initialized":
					w.WriteHeader(202)
				case "tools/list":
					mcpHTTPReply(w, req.ID, map[string]any{"tools": []any{supervisorTool("tool")}})
				case "tools/call":
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			policy := supervisorPolicy()
			if !stable {
				policy.MaxDelay = time.Second
			}
			s, err := OpenSupervisor(context.Background(), SupervisorOptions{Name: "server", CWD: t.TempDir(), Server: state.MCPServer{URL: server.URL}, Reconnect: policy})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			first := waitSupervisor(t, s, func(v SupervisorSnapshot) bool { return v.Status == "connected" })
			if first.Attempts != 1 {
				t.Fatalf("initial recovery attempt lost: %+v", first)
			}
			if stable {
				time.Sleep(2 * policy.MaxDelay)
			}
			_, _ = s.CallTool(context.Background(), "tool", json.RawMessage(`{}`))
			closeTestTransport(s)
			next := waitSupervisor(t, s, func(v SupervisorSnapshot) bool { return v.Status == "connected" && v.Generation > first.Generation })
			if stable {
				if next.Attempts != 1 {
					t.Fatalf("stability did not reset budget: %+v", next)
				}
			} else {
				if next.Attempts != 2 {
					t.Fatalf("brief success reset budget: %+v", next)
				}
				_, _ = s.CallTool(context.Background(), "tool", json.RawMessage(`{}`))
				closeTestTransport(s)
				waitSupervisor(t, s, func(v SupervisorSnapshot) bool { return v.Status == "exhausted" })
				if initializes.Load() != 3 {
					t.Fatalf("crash loop exceeded budget: %d", initializes.Load())
				}
			}
		})
	}
}

func TestSupervisorTrimsInstructionsBeforeAttributedByteLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(405)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(204)
			return
		}
		var req wire
		_ = json.NewDecoder(r.Body).Decode(&req)
		if legacyProbeReply(w, req) {
			return
		}
		switch req.Method {
		case "initialize":
			supervisorInitialize(w, req, 1, "ok"+strings.Repeat(" ", 40000)+"\ufeff")
		case "notifications/initialized":
			w.WriteHeader(202)
		case "tools/list":
			mcpHTTPReply(w, req.ID, map[string]any{"tools": []any{}})
		}
	}))
	defer server.Close()
	expected := "### MCP server: server\n\nok"
	s, err := OpenSupervisor(context.Background(), SupervisorOptions{Name: "server", CWD: t.TempDir(), Server: state.MCPServer{URL: server.URL}, MaxInstructionBytes: len([]byte(expected)), FailOnStartupError: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := s.Snapshot().Instructions; got != expected {
		t.Fatalf("trimmed instructions %q", got)
	}
}
