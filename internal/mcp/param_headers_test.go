package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"dsgo/internal/state"
)

func TestMCPParamHeaderDeclarationConstraints(t *testing.T) {
	for _, schema := range []string{
		`{"type":"object","x-mcp-header":"Root"}`,
		`{"type":"object","properties":{"a":{"type":"string","x-mcp-header":"Dup"},"b":{"type":"integer","x-mcp-header":"dup"}}}`,
		`{"type":"object","properties":{"a":{"type":"object","x-mcp-header":"Object"}}}`,
		`{"type":"object","properties":{"a":{"type":"string","x-mcp-header":"Bad Name"}}}`,
		`{"type":"object","properties":{"a":{"anyOf":[{"type":"string","x-mcp-header":"Branch"}]}}}`,
		`{"type":"object","additionalProperties":{"type":"string","x-mcp-header":"Additional"}}`,
		`{"type":"object","$defs":{"unused":{"type":"string","x-mcp-header":"Definition"}}}`,
		`{"type":"object","properties":{"a":{"type":["string","null"],"x-mcp-header":"Union"}}}`,
	} {
		if _, err := scanParamHeaders(json.RawMessage(schema)); err == nil {
			t.Fatalf("invalid header declaration accepted: %s", schema)
		}
	}
	declarations, err := scanParamHeaders(json.RawMessage(`{"type":"object","properties":{"a":{"type":"number","x-mcp-header":"Number"},"nested":{"type":"object","properties":{"b":{"type":"boolean","x-mcp-header":"Flag"}}}}}`))
	if err != nil || len(declarations) != 2 {
		t.Fatalf("valid declarations rejected: %v %v", declarations, err)
	}
}

func TestMCPParamHeaderPrimitiveAndSentinelEncoding(t *testing.T) {
	declarations := []paramHeaderDeclaration{}
	for _, name := range []string{"plain", "unicode", "empty", "leading", "sentinel", "count", "flag", "negativeZero", "fraction", "unsafe", "null", "missing"} {
		declarations = append(declarations, paramHeaderDeclaration{Path: []string{name}, Name: name})
	}
	declarations = append(declarations, paramHeaderDeclaration{Path: []string{"nested", "value"}, Name: "nested"})
	ctx := withParamHeaders(context.Background(), declarations, json.RawMessage(`{"plain":"project value","unicode":"中文","empty":"","leading":" leading","sentinel":"=?base64?abc?=","count":42,"flag":false,"negativeZero":-0,"fraction":0.0000001,"unsafe":9007199254740992,"null":null,"nested":{"value":true}}`))
	headers, _ := ctx.Value(paramHeadersContextKey{}).(map[string]string)
	want := map[string]string{"Mcp-Param-plain": "project value", "Mcp-Param-unicode": encodeHeaderValue("中文"), "Mcp-Param-empty": "=?base64??=", "Mcp-Param-leading": encodeHeaderValue(" leading"), "Mcp-Param-sentinel": encodeHeaderValue("=?base64?abc?="), "Mcp-Param-count": "42", "Mcp-Param-flag": "false", "Mcp-Param-negativeZero": "0", "Mcp-Param-fraction": "1e-7", "Mcp-Param-nested": "true"}
	if !reflect.DeepEqual(headers, want) {
		t.Fatalf("headers %+v want %+v", headers, want)
	}
}

func TestModernHTTPParamHeadersFilterAndExplicitContinuation(t *testing.T) {
	for _, supervised := range []bool{false, true} {
		t.Run(map[bool]string{false: "client", true: "supervisor"}[supervised], func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer configured" {
					t.Error("custom param headers overwrote authorization")
				}
				var req wire
				_ = json.NewDecoder(r.Body).Decode(&req)
				switch req.Method {
				case "server/discover":
					mcpHTTPReply(w, req.ID, modernDiscover(false))
				case "tools/list":
					mcpHTTPReply(w, req.ID, cachedResult(map[string]any{"tools": []any{
						map[string]any{"name": "good", "inputSchema": json.RawMessage(`{"type":"object","properties":{"project":{"type":"string","x-mcp-header":"Project"},"nested":{"type":"object","properties":{"count":{"type":"integer","x-mcp-header":"Count"}}},"credential":{"type":"string","x-mcp-header":"Authorization"}}}`)},
						map[string]any{"name": "bad", "inputSchema": json.RawMessage(`{"type":"object","properties":{"a":{"type":"string","x-mcp-header":"Duplicate"},"b":{"type":"string","x-mcp-header":"duplicate"}}}`)},
					}}))
				case "tools/call":
					if r.Header.Get("Mcp-Param-Project") != encodeHeaderValue("中文") || r.Header.Get("Mcp-Param-Count") != "3" || r.Header.Get("Mcp-Param-Authorization") != "parameter" {
						t.Errorf("missing mirrored params: %v", r.Header)
					}
					if calls.Add(1) == 1 {
						mcpHTTPReply(w, req.ID, map[string]any{"resultType": "input_required", "requestState": "continue"})
						return
					}
					mcpHTTPReply(w, req.ID, completeResult(map[string]any{"content": []any{map[string]string{"type": "text", "text": "done"}}}))
				default:
					w.WriteHeader(400)
				}
			}))
			defer server.Close()
			config := state.MCPServer{URL: server.URL, Headers: map[string]string{"Authorization": "Bearer configured"}}
			args := json.RawMessage(`{"project":"中文","nested":{"count":3},"credential":"parameter"}`)
			if supervised {
				s, err := OpenSupervisor(context.Background(), SupervisorOptions{Name: "headers", CWD: t.TempDir(), Server: config})
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				if tools := s.Snapshot().Tools; len(tools) != 1 || tools[0].Name != "good" {
					t.Fatalf("invalid declaration offered: %+v", tools)
				}
				if _, err := s.CallTool(context.Background(), "good", args); err != nil {
					t.Fatal(err)
				}
			} else {
				client, err := Open(context.Background(), t.TempDir(), config)
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				tools, err := client.ListTools(context.Background())
				if err != nil || len(tools) != 1 || tools[0].Name != "good" {
					t.Fatalf("invalid declaration offered: %+v %v", tools, err)
				}
				if _, err := client.CallTool(context.Background(), "good", args); err != nil {
					t.Fatal(err)
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("unexpected retries: %d", calls.Load())
			}
		})
	}
}
