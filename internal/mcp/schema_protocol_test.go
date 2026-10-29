package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"dsgo/internal/core"
	"dsgo/internal/state"
)

func TestProtocolNegotiationAndAdvertisedOutputValidation(t *testing.T) {
	for _, version := range []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05", "2024-10-07"} {
		t.Run(version, func(t *testing.T) {
			var calls atomic.Int32
			var deleted atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.WriteHeader(405)
					return
				}
				if r.Method == "DELETE" {
					if got := r.Header.Get("MCP-Protocol-Version"); got != version {
						t.Errorf("DELETE protocol %q, want %q", got, version)
					}
					deleted.Store(true)
					w.WriteHeader(204)
					return
				}
				var request wire
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				if len(request.ID) == 0 {
					w.WriteHeader(202)
					return
				}
				if legacyProbeReply(w, request) {
					return
				}
				var result any
				switch request.Method {
				case "initialize":
					if got := r.Header.Get("MCP-Protocol-Version"); got != "" {
						t.Errorf("initial header %q", got)
					}
					w.Header().Set("Mcp-Session-Id", "negotiated")
					result = map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "schema", "version": "1"}}
				case "tools/list":
					result = map[string]any{"tools": []any{
						map[string]any{"name": "count", "inputSchema": map[string]any{"type": "object"}, "outputSchema": map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"type": "integer", "minimum": 1}}, "required": []string{"n"}, "additionalProperties": false}},
						map[string]any{"name": "task", "inputSchema": map[string]any{"type": "object"}, "execution": map[string]any{"taskSupport": "required"}},
					}}
				case "tools/call":
					calls.Add(1)
					var params struct{ Arguments map[string]any }
					json.Unmarshal(request.Params, &params)
					result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "counted"}}, "structuredContent": params.Arguments}
				default:
					t.Errorf("unexpected method %s", request.Method)
					w.WriteHeader(400)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(wire{JSONRPC: "2.0", ID: request.ID, Result: core.JSON(result)})
			}))
			defer srv.Close()
			client, err := Open(context.Background(), t.TempDir(), state.MCPServer{URL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = client.ListTools(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err = client.CallTool(context.Background(), "count", json.RawMessage(`{"n":2}`)); err != nil {
				t.Fatal(err)
			}
			if _, err = client.CallTool(context.Background(), "count", json.RawMessage(`{"n":0}`)); err == nil || !strings.Contains(err.Error(), "advertised schema") {
				t.Fatalf("invalid output accepted: %v", err)
			}
			before := calls.Load()
			if _, err = client.CallTool(context.Background(), "task", json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "task-based") {
				t.Fatalf("required task executed: %v", err)
			}
			if calls.Load() != before {
				t.Fatal("unsupported task was sent to server")
			}
			if err = client.Close(); err != nil {
				t.Fatal(err)
			}
			if !deleted.Load() {
				t.Fatal("HTTP session not closed")
			}
		})
	}
}

func TestInvalidMCPStructuredContentAndBlocks(t *testing.T) {
	for _, result := range []ToolResult{
		{Content: []json.RawMessage{json.RawMessage(`{"type":"text","text":null}`)}},
		{Content: []json.RawMessage{json.RawMessage(`{"type":"image","data":3,"mimeType":"image/png"}`)}},
		{Content: []json.RawMessage{json.RawMessage(`"not an object"`)}},
		{Content: []json.RawMessage{}, StructuredContent: json.RawMessage(`[]`)},
	} {
		if err := validateToolResult(result); err == nil {
			t.Fatalf("invalid result accepted: %+v", result)
		}
	}
}
