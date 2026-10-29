package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"dsgo/internal/state"
)

func TestSDKWireResultCorpus(t *testing.T) {
	data, err := os.ReadFile("testdata/wire-corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Cases []struct {
			Name, Era, Method string
			Value             json.RawMessage
			Valid             bool
		}
	}
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	for _, test := range corpus.Cases {
		t.Run(test.Era+"/"+test.Name, func(t *testing.T) {
			err := validateResultWire(test.Method, test.Value, test.Era == "modern")
			if (err == nil) != test.Valid {
				t.Fatalf("valid=%v, want %v: %v", err == nil, test.Valid, err)
			}
		})
	}
}

func TestMCPInputSchemaIsRetainedWithoutLocalCompilation(t *testing.T) {
	for _, modern := range []bool{false, true} {
		name := "legacy"
		if modern {
			name = "modern"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.WriteHeader(405)
					return
				}
				var req wire
				_ = json.NewDecoder(r.Body).Decode(&req)
				if req.Method == "server/discover" {
					if modern {
						mcpHTTPReply(w, req.ID, modernDiscover(false))
					} else {
						legacyProbeReply(w, req)
					}
					return
				}
				if req.Method == "initialize" {
					supervisorInitialize(w, req, 1, "")
					return
				}
				if len(req.ID) == 0 {
					w.WriteHeader(202)
					return
				}
				switch req.Method {
				case "tools/list":
					value := map[string]any{"tools": []any{map[string]any{"name": "", "inputSchema": map[string]any{"type": "object", "$ref": "https://invalid.test/remote", "properties": map[string]any{"n": map[string]string{"type": "unknown"}}}}}}
					if modern {
						value = cachedResult(value)
					}
					mcpHTTPReply(w, req.ID, value)
				case "tools/call":
					value := map[string]any{"content": []any{map[string]string{"type": "text", "text": "server received"}}}
					if modern {
						value = completeResult(value)
					}
					mcpHTTPReply(w, req.ID, value)
				}
			}))
			defer server.Close()
			client, err := Open(context.Background(), t.TempDir(), state.MCPServer{URL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			tools, err := client.ListTools(context.Background())
			if err != nil || len(tools) != 1 || tools[0].Name != "" {
				t.Fatalf("raw tool schema rejected: %+v %v", tools, err)
			}
			if _, err := client.CallTool(context.Background(), "", json.RawMessage(`{"n":"server-owned validation"}`)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLegacyContentlessToolResultNormalizesOnlyPlainFamily(t *testing.T) {
	c := &Client{ProtocolVersion: LegacyProtocolVersion}
	for _, raw := range []string{`{}`, `{"structuredContent":{"n":2}}`, `{"isError":true}`} {
		var result ToolResult
		if err := c.decodeResult("tools/call", json.RawMessage(raw), &result, false); err != nil || result.Content == nil {
			t.Fatalf("contentless %s: %+v %v", raw, result, err)
		}
	}
	for _, raw := range []string{`{"task":{}}`, `{"requestState":"state"}`, `{"inputRequests":{}}`, `{"content":null}`} {
		if err := c.decodeResult("tools/call", json.RawMessage(raw), nil, false); err == nil {
			t.Fatalf("foreign family accepted: %s", raw)
		}
	}
}

func TestWireValidationRunsBeforeCacheAndToolProjection(t *testing.T) {
	c := &Client{ProtocolVersion: ProtocolVersion}
	for _, raw := range []string{
		`{"resultType":"complete","content":[{"type":"unknown"}]}`,
		`{"resultType":"complete","isError":true,"content":[{"type":"audio","data":"***","mimeType":"audio/test"}]}`,
		`{"resultType":"complete","ttlMs":1000,"cacheScope":"private","contents":[{"uri":"test://one","blob":"x"}]}`,
	} {
		method := "tools/call"
		if strings.Contains(raw, `"contents"`) {
			method = "resources/read"
		}
		if err := c.decodeResult(method, json.RawMessage(raw), nil, false); err == nil {
			t.Fatalf("invalid wire admitted: %s", raw)
		}
	}
}
