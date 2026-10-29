package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dsgo/internal/state"
)

func modernDiscover(listChanged bool) map[string]any {
	return map[string]any{
		"supportedVersions": []string{ProtocolVersion}, "capabilities": map[string]any{"tools": map[string]bool{"listChanged": listChanged}}, "instructions": "modern literal {instructions}", "_meta": map[string]any{serverInfoMetaKey: map[string]string{"name": "modern", "version": "1"}},
	}
}
func completeResult(value map[string]any) map[string]any {
	value["resultType"] = "complete"
	return value
}
func cachedResult(value map[string]any) map[string]any {
	value["ttlMs"] = 0
	value["cacheScope"] = "private"
	return completeResult(value)
}
func assertModernEnvelope(t *testing.T, req wire, headers http.Header) {
	t.Helper()
	var params map[string]json.RawMessage
	var meta map[string]json.RawMessage
	if json.Unmarshal(req.Params, &params) != nil || json.Unmarshal(params["_meta"], &meta) != nil {
		t.Errorf("missing envelope: %s", req.Params)
		return
	}
	var version string
	_ = json.Unmarshal(meta[protocolMetaKey], &version)
	var capabilities map[string]any
	_ = json.Unmarshal(meta[capabilitiesMetaKey], &capabilities)
	var info map[string]string
	_ = json.Unmarshal(meta[clientInfoMetaKey], &info)
	if version != ProtocolVersion || capabilities == nil || len(capabilities) != 0 || info["name"] != "dsgo" {
		t.Errorf("wrong envelope: %s", req.Params)
	}
	if headers != nil {
		if headers.Get("MCP-Protocol-Version") != ProtocolVersion {
			t.Errorf("wrong version header: %v", headers)
		}
		if len(req.ID) > 0 && headers.Get("Mcp-Method") != req.Method {
			t.Errorf("wrong method header: %v", headers)
		}
	}
}

func TestModernHTTPDiscoverEnvelopeSubscriptionAndRawJSON(t *testing.T) {
	notices := make(chan wire, 4)
	listenClosed := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	var probes, initializes, calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected modern method %s", r.Method)
			w.WriteHeader(405)
			return
		}
		var req wire
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(400)
			return
		}
		assertModernEnvelope(t, req, r.Header)
		switch req.Method {
		case "server/discover":
			probes.Add(1)
			if string(req.ID) != `"server-discover-probe-1"` {
				t.Errorf("probe id %s", req.ID)
			}
			mcpHTTPReply(w, req.ID, modernDiscover(true))
		case "initialize", "notifications/initialized":
			initializes.Add(1)
			w.WriteHeader(500)
		case "subscriptions/listen":
			if string(req.ID) != `"listen:0"` {
				t.Errorf("listen id %s", req.ID)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			ack := wire{JSONRPC: "2.0", Method: "notifications/subscriptions/acknowledged", Params: mustJSON(map[string]any{"_meta": map[string]any{subscriptionMetaKey: "listen:0"}, "notifications": map[string]bool{"toolsListChanged": true}})}
			data, _ := json.Marshal(ack)
			fmt.Fprintf(w, "data: %s\n\n", data)
			w.(http.Flusher).Flush()
			for {
				select {
				case <-r.Context().Done():
					close(listenClosed)
					return
				case notice := <-notices:
					data, _ := json.Marshal(notice)
					fmt.Fprintf(w, "data: %s\n\n", data)
					w.(http.Flusher).Flush()
				}
			}
		case "tools/list":
			mcpHTTPReply(w, req.ID, cachedResult(map[string]any{"tools": []any{map[string]any{"name": "工具/a", "inputSchema": map[string]any{"type": "object"}, "outputSchema": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}}}}))
		case "tools/call":
			calls.Add(1)
			if got := r.Header.Get("Mcp-Name"); got != encodeHeaderValue("工具/a") {
				t.Errorf("tool name header %q", got)
			}
			mcpHTTPReply(w, req.ID, completeResult(map[string]any{"content": []any{map[string]string{"type": "text", "text": "done"}}, "structuredContent": []int{1, 2}}))
		case "notifications/cancelled":
			var params struct {
				RequestID string `json:"requestId"`
			}
			_ = json.Unmarshal(req.Params, &params)
			if params.RequestID != "listen:0" {
				t.Errorf("cancel id %s", req.Params)
			}
			cancelled <- struct{}{}
			w.WriteHeader(202)
		default:
			t.Errorf("unexpected request %s", req.Method)
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	client, err := Open(context.Background(), t.TempDir(), state.MCPServer{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if client.ProtocolVersion != ProtocolVersion || client.ServerInfo["name"] != "modern" || client.Instructions != "modern literal {instructions}" {
		t.Fatalf("modern discovery not adopted: %+v", client.ServerInfo)
	}
	if _, err := client.ListTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := client.CallTool(context.Background(), "工具/a", json.RawMessage(`{}`))
	if err != nil || string(result.StructuredContent) != "[1,2]" {
		t.Fatalf("natural JSON result %+v %v", result, err)
	}
	notices <- wire{JSONRPC: "2.0", Method: "notifications/tools/list_changed", Params: mustJSON(map[string]any{"_meta": map[string]any{subscriptionMetaKey: "wrong"}})}
	select {
	case <-client.toolsChanged:
		t.Fatal("foreign subscription notification accepted")
	case <-time.After(10 * time.Millisecond):
	}
	notices <- wire{JSONRPC: "2.0", Method: "notifications/tools/list_changed", Params: mustJSON(map[string]any{"_meta": map[string]any{subscriptionMetaKey: "listen:0"}})}
	select {
	case <-client.toolsChanged:
	case <-time.After(time.Second):
		t.Fatal("matching subscription change lost")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("subscription cancel notification missing")
	}
	select {
	case <-listenClosed:
	case <-time.After(time.Second):
		t.Fatal("subscription stream survived close")
	}
	if probes.Load() != 1 || initializes.Load() != 0 || calls.Load() != 1 {
		t.Fatalf("wire counts: probes %d initialize %d calls %d", probes.Load(), initializes.Load(), calls.Load())
	}
}

func TestModernHTTPOrdinaryCancellationDoesNotPostCancelled(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	var cancelled, resumes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			resumes.Add(1)
			w.WriteHeader(405)
			return
		}
		var req wire
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "server/discover":
			mcpHTTPReply(w, req.ID, modernDiscover(false))
		case "tools/call":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "id: pending\nretry: 1\n\n")
			w.(http.Flusher).Flush()
			close(started)
			<-r.Context().Done()
			close(stopped)
		case "notifications/cancelled":
			cancelled.Add(1)
			w.WriteHeader(202)
		default:
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	client, err := Open(context.Background(), t.TempDir(), state.MCPServer{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := client.CallTool(ctx, "slow", json.RawMessage(`{}`)); result <- err }()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel returned %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("ordinary stream survived cancellation")
	}
	if cancelled.Load() != 0 || resumes.Load() != 0 {
		t.Fatalf("cancel generated wire work: cancelled=%d resumed=%d", cancelled.Load(), resumes.Load())
	}
}

func TestModernListenRefusalIsSoftAndResultsAreDiscriminated(t *testing.T) {
	var mode atomic.Int64
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req wire
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "server/discover":
			mcpHTTPReply(w, req.ID, modernDiscover(true))
		case "subscriptions/listen":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(wire{JSONRPC: "2.0", ID: req.ID, Error: &RPCError{Code: -32000, Message: "capacity exhausted"}})
		case "tools/call":
			calls.Add(1)
			value := map[string]any{"content": []any{}}
			switch mode.Load() {
			case 1:
				value["resultType"] = "future"
			case 2:
				value["resultType"] = "input_required"
				value["inputRequests"] = map[string]any{"request": map[string]any{"method": "roots/list"}}
			case 3:
				value["resultType"] = "complete"
				value["structuredContent"] = false
			}
			mcpHTTPReply(w, req.ID, value)
		default:
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	client, err := Open(context.Background(), t.TempDir(), state.MCPServer{URL: server.URL})
	if err != nil {
		t.Fatalf("listen refusal aborted connection: %v", err)
	}
	defer client.Close()
	for n := int64(0); n < 4; n++ {
		mode.Store(n)
		result, err := client.CallTool(context.Background(), "tool", json.RawMessage(`{}`))
		switch n {
		case 0:
			if err == nil || !strings.Contains(err.Error(), "resultType") {
				t.Fatalf("missing discriminator accepted: %v", err)
			}
		case 1:
			if err == nil || !strings.Contains(err.Error(), "resultType") {
				t.Fatalf("unknown discriminator accepted: %v", err)
			}
		case 2:
			var required *InputRequiredError
			if !errors.As(err, &required) {
				t.Fatalf("input_required masked: %v", err)
			}
		case 3:
			if err != nil || string(result.StructuredContent) != "false" {
				t.Fatalf("primitive rejected: %+v %v", result, err)
			}
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("calls unexpectedly retried: %d", calls.Load())
	}
}

func TestModernHTTPProbeClassificationAndCorrectiveLoop(t *testing.T) {
	for _, test := range []struct {
		name               string
		status             int
		corrective, repeat bool
		wantError          bool
	}{
		{"auth", 401, false, false, true}, {"forbidden", 403, false, false, true}, {"server-error", 503, false, false, true}, {"corrective", 400, true, false, false}, {"corrective-loop", 400, true, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var probes, initializes atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req wire
				_ = json.NewDecoder(r.Body).Decode(&req)
				if req.Method == "initialize" {
					initializes.Add(1)
					w.WriteHeader(500)
					return
				}
				n := probes.Add(1)
				if test.corrective && n > 1 && !test.repeat {
					mcpHTTPReply(w, req.ID, modernDiscover(false))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				if test.corrective {
					_ = json.NewEncoder(w).Encode(wire{JSONRPC: "2.0", ID: req.ID, Error: &RPCError{Code: -32022, Message: "choose mutual version", Data: mustJSON(map[string]any{"supported": []string{ProtocolVersion}})}})
				}
			}))
			defer server.Close()
			client, err := Open(context.Background(), t.TempDir(), state.MCPServer{URL: server.URL})
			if (err != nil) != test.wantError {
				if client != nil {
					client.Close()
				}
				t.Fatalf("unexpected result %v", err)
			}
			if client != nil {
				client.Close()
			}
			expected := int64(1)
			if test.corrective {
				expected = 2
			}
			if probes.Load() != expected || initializes.Load() != 0 {
				t.Fatalf("wrong negotiation flow probes=%d initializes=%d", probes.Load(), initializes.Load())
			}
		})
	}
}
