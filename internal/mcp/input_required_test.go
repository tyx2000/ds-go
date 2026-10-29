package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"dsgo/internal/state"
)

func TestMCPInputRequiredStateOnlyContinuationPreservesParams(t *testing.T) {
	for _, stateText := range []string{`"opaque\u0000\ud800\"state"`, `""`} {
		t.Run(stateText, func(t *testing.T) {
			var calls, effects atomic.Int64
			var firstID atomic.Value
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request wire
				_ = json.NewDecoder(r.Body).Decode(&request)
				if request.Method == "server/discover" {
					mcpHTTPReply(w, request.ID, modernDiscover(false))
					return
				}
				if request.Method != "tools/call" {
					w.WriteHeader(400)
					return
				}
				assertModernEnvelope(t, request, r.Header)
				var params map[string]json.RawMessage
				_ = json.Unmarshal(request.Params, &params)
				if string(params["arguments"]) != `{"x":1}` {
					t.Errorf("original arguments changed: %s", params["arguments"])
				}
				var meta map[string]json.RawMessage
				_ = json.Unmarshal(params["_meta"], &meta)
				if string(meta["custom"]) != `"preserve"` {
					t.Errorf("original meta changed: %s", params["_meta"])
				}
				if calls.Add(1) == 1 {
					firstID.Store(string(request.ID))
					mcpHTTPReply(w, request.ID, map[string]any{"resultType": "input_required", "requestState": json.RawMessage(stateText)})
					return
				}
				if string(params["requestState"]) != stateText {
					t.Errorf("opaque state changed: got %s want%s", params["requestState"], stateText)
				}
				if firstID.Load().(string) == string(request.ID) {
					t.Error("continuation reused request id")
				}
				effects.Add(1)
				mcpHTTPReply(w, request.ID, completeResult(map[string]any{"content": []any{map[string]string{"type": "text", "text": "done"}}}))
			}))
			defer server.Close()
			client, err := Open(context.Background(), t.TempDir(), state.MCPServer{URL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			started := time.Now()
			var result ToolResult
			err = client.Call(context.Background(), "tools/call", map[string]any{"name": "effect", "arguments": json.RawMessage(`{"x":1}`), "_meta": map[string]string{"custom": "preserve"}}, &result)
			if err != nil || len(result.Content) != 1 {
				t.Fatalf("continuation result %+v %v", result, err)
			}
			if calls.Load() != 2 || effects.Load() != 1 || time.Since(started) < 200*time.Millisecond {
				t.Fatalf("unpaced/repeated effect: calls=%d effects=%d elapsed=%s", calls.Load(), effects.Load(), time.Since(started))
			}
		})
	}
}

func TestMCPInputRequiredRoundCapAndUnsupportedCapabilities(t *testing.T) {
	for _, mode := range []string{"round-cap", "no-handler", "unknown-input", "http-error", "state-then-http-error", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			entered := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request wire
				_ = json.NewDecoder(r.Body).Decode(&request)
				if request.Method == "server/discover" {
					mcpHTTPReply(w, request.ID, modernDiscover(false))
					return
				}
				n := calls.Add(1)
				if mode == "http-error" || mode == "state-then-http-error" && n > 1 {
					w.WriteHeader(503)
					return
				}
				value := map[string]any{"resultType": "input_required", "requestState": "busy"}
				if mode == "no-handler" {
					value["inputRequests"] = map[string]any{"required": map[string]string{"method": "roots/list"}}
				}
				if mode == "unknown-input" {
					value["inputRequests"] = map[string]any{"required": map[string]string{"method": "ping"}}
				}
				mcpHTTPReply(w, request.ID, value)
				select {
				case entered <- struct{}{}:
				default:
				}
			}))
			defer server.Close()
			client, err := Open(context.Background(), t.TempDir(), state.MCPServer{URL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := client.CallTool(ctx, "effect", json.RawMessage(`{}`)); done <- err }()
			if mode == "cancel" {
				<-entered
				time.Sleep(25 * time.Millisecond)
				cancel()
			}
			err = <-done
			if err == nil {
				t.Fatal("incomplete/failed request became success")
			}
			want := int64(1)
			switch mode {
			case "round-cap":
				want = 11
				var exceeded *InputRoundsExceededError
				if !errors.As(err, &exceeded) {
					t.Fatalf("round cap: %v", err)
				}
			case "no-handler":
				var unsupported *InputCapabilityError
				if !errors.As(err, &unsupported) {
					t.Fatalf("missing handler: %v", err)
				}
			case "state-then-http-error":
				want = 2
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled continuation: %v", err)
				}
			}
			if calls.Load() != want {
				t.Fatalf("mode%s requests=%d want%d (unexpected replay)", mode, calls.Load(), want)
			}
		})
	}
}
