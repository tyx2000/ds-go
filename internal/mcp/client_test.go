package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"dsgo/internal/state"
)

func TestMCPServerHelper(t *testing.T) {
	if os.Getenv("DSGO_MCP_TEST_CHILD") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), stdioReadBufferBytes)
	enc := json.NewEncoder(os.Stdout)
	initialized := false
	cancelled := false
	for scanner.Scan() {
		var req wire
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			os.Exit(2)
		}
		var result any
		switch req.Method {
		case "server/discover":
			_ = enc.Encode(wire{JSONRPC: "2.0", ID: req.ID, Error: &RPCError{Code: -32601, Message: "legacy server"}})
			continue
		case "initialize":
			result = map[string]any{"protocolVersion": LegacyProtocolVersion, "capabilities": map[string]any{"tools": map[string]bool{}, "resources": map[string]bool{}}, "serverInfo": map[string]string{"name": "test", "version": "1"}, "instructions": "Literal {instructions}"}
		case "notifications/initialized":
			initialized = true
			continue
		case "notifications/cancelled":
			cancelled = true
			continue
		case "tools/list":
			if !initialized {
				os.Exit(3)
			}
			var params map[string]string
			_ = json.Unmarshal(req.Params, &params)
			if params["cursor"] == "second" {
				result = map[string]any{"tools": []map[string]any{{"name": "second", "description": "page two", "inputSchema": map[string]string{"type": "object"}}}}
			} else {
				result = map[string]any{"tools": []map[string]any{{"name": "echo", "description": "echo", "inputSchema": map[string]string{"type": "object"}}}, "nextCursor": "second"}
			}
		case "resources/list":
			result = map[string]any{"resources": []map[string]string{{"uri": "test://doc", "name": "doc"}}}
		case "resources/templates/list":
			result = map[string]any{"resourceTemplates": []map[string]string{{"uriTemplate": "test://{name}", "name": "docs"}}}
		case "resources/read":
			result = map[string]any{"contents": []map[string]string{{"uri": "test://doc", "text": "resource body"}}}
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &params)
			if params.Name == "slow" {
				continue
			}
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": params.Arguments["text"]}}, "isError": params.Name == "failure"}
		case "test/cancelled":
			result = map[string]bool{"cancelled": cancelled}
		case "test/env":
			result = map[string]string{"secret": os.Getenv("AMBIENT_TEST_SECRET"), "ordinary": os.Getenv("ORDINARY_TEST_VALUE"), "override": os.Getenv("EXPLICIT_TOKEN")}
		case "test/echo":
			result = json.RawMessage(req.Params)
		default:
			_ = enc.Encode(wire{JSONRPC: "2.0", ID: req.ID, Error: &RPCError{Code: -32601, Message: "unknown method"}})
			continue
		}
		b, _ := json.Marshal(result)
		_ = enc.Encode(wire{JSONRPC: "2.0", ID: req.ID, Result: b})
	}
	os.Exit(0)
}

func openHelper(t *testing.T) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := Open(ctx, t.TempDir(), state.MCPServer{Command: os.Args[0], Args: []string{"-test.run=^TestMCPServerHelper$"}, Env: map[string]string{"DSGO_MCP_TEST_CHILD": "1", "EXPLICIT_TOKEN": "kept"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c
}

func TestStdioLifecyclePaginationResourcesAndCancellation(t *testing.T) {
	t.Setenv("AMBIENT_TEST_SECRET", "must-not-inherit")
	t.Setenv("ORDINARY_TEST_VALUE", "visible")
	c := openHelper(t)
	if c.Instructions != "Literal {instructions}" {
		t.Fatal("server instructions changed")
	}
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 || tools[1].Name != "second" {
		t.Fatalf("pagination: %+v", tools)
	}
	resources, err := c.ListResources(context.Background())
	if err != nil || len(resources) != 1 {
		t.Fatalf("resources %v %v", resources, err)
	}
	templates, err := c.ListResourceTemplates(context.Background())
	if err != nil || len(templates) != 1 {
		t.Fatalf("templates %v %v", templates, err)
	}
	var resource any
	if err := c.Call(context.Background(), "resources/read", map[string]string{"uri": "test://doc"}, &resource); err != nil {
		t.Fatal(err)
	}
	var env map[string]string
	if err := c.Call(context.Background(), "test/env", nil, &env); err != nil {
		t.Fatal(err)
	}
	if env["secret"] != "" || env["ordinary"] != "visible" || env["override"] != "kept" {
		t.Fatalf("bad env scrubbing: %+v", env)
	}
	result, err := c.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"hello"}`))
	if err != nil || len(result.Content) != 1 {
		t.Fatalf("call result %+v %v", result, err)
	}
	result, err = c.CallTool(context.Background(), "failure", json.RawMessage(`{"text":"failure"}`))
	if err == nil || !result.IsError {
		t.Fatal("isError became success")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.CallTool(ctx, "slow", json.RawMessage(`{}`)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel: %v", err)
	}
	var status map[string]bool
	if err := c.Call(context.Background(), "test/cancelled", nil, &status); err != nil {
		t.Fatal(err)
	}
	if !status["cancelled"] {
		t.Fatal("cancellation notification missing")
	}
}

func TestConcurrentStdioCallsMatchIDs(t *testing.T) {
	c := openHelper(t)
	var wg sync.WaitGroup
	for n := 0; n < 20; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			var reply map[string]int
			if err := c.Call(context.Background(), "test/echo", map[string]int{"n": n}, &reply); err != nil {
				t.Error(err)
				return
			}
			if reply["n"] != n {
				t.Errorf("wrong response %d: %+v", n, reply)
			}
		}(n)
	}
	wg.Wait()
}

func TestHTTPJSONSSEAndSessionHeaders(t *testing.T) {
	var mu sync.Mutex
	initialized := false
	deleted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(405)
			return
		}
		if r.Header.Get("Authorization") != "Bearer configured" {
			t.Error("configured header missing")
		}
		if r.Method == http.MethodDelete {
			mu.Lock()
			deleted = true
			mu.Unlock()
			w.WriteHeader(204)
			return
		}
		if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			t.Error("SSE not accepted")
		}
		var req wire
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if legacyProbeReply(w, req) {
			return
		}
		if req.Method == "initialize" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "session-1")
			b, _ := json.Marshal(map[string]any{"protocolVersion": LegacyProtocolVersion, "capabilities": map[string]any{"tools": map[string]bool{}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}})
			_ = json.NewEncoder(w).Encode(wire{JSONRPC: "2.0", ID: req.ID, Result: b})
			return
		}
		if r.Header.Get("Mcp-Session-Id") != "session-1" {
			t.Error("session header missing")
		}
		if req.Method == "notifications/initialized" {
			mu.Lock()
			initialized = true
			mu.Unlock()
			w.WriteHeader(202)
			return
		}
		mu.Lock()
		ready := initialized
		mu.Unlock()
		if !ready {
			t.Error("operation before initialized")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if req.Method == "tools/list" {
			fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{\"level\":\"info\"}}\n\n")
			fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"tools\":[]}}\n\n", req.ID)
			return
		}
		fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"http\"}]}}\n\n", req.ID)
	}))
	defer server.Close()
	c, err := Open(context.Background(), t.TempDir(), state.MCPServer{URL: server.URL, Headers: map[string]string{"Authorization": "Bearer configured"}})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := c.ListTools(context.Background())
	if err != nil || len(tools) != 0 {
		t.Fatalf("HTTP tools %+v %v", tools, err)
	}
	select {
	case n := <-c.Notifications():
		if n.Method != "notifications/message" {
			t.Fatal(n.Method)
		}
	default:
		t.Fatal("HTTP notification dropped")
	}
	if _, err := c.CallTool(context.Background(), "echo", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !deleted {
		t.Fatal("HTTP session was not deleted")
	}
}

func TestMalformedFramesAndRepeatedPageTerminates(t *testing.T) {
	c := &Client{pending: map[string]chan response{}, notifications: make(chan Notification, 1)}
	for _, frame := range []string{`{oops}`, `{"jsonrpc":"1.0","id":1,"result":{}}`, `{"jsonrpc":"2.0","id":1}`, `[]`} {
		if err := c.handle([]byte(frame), func(wire) error { return nil }); err == nil {
			t.Errorf("accepted malformed %s", frame)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(405)
			return
		}
		var req wire
		_ = json.NewDecoder(r.Body).Decode(&req)
		if legacyProbeReply(w, req) {
			return
		}
		if req.Method == "notifications/initialized" {
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var result any
		if req.Method == "initialize" {
			result = map[string]any{"protocolVersion": LegacyProtocolVersion, "capabilities": map[string]any{"tools": map[string]bool{}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
		} else {
			result = map[string]any{"tools": []any{}, "nextCursor": "repeat"}
		}
		b, _ := json.Marshal(result)
		_ = json.NewEncoder(w).Encode(wire{JSONRPC: "2.0", ID: req.ID, Result: b})
	}))
	defer server.Close()
	client, err := Open(context.Background(), t.TempDir(), state.MCPServer{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if tools, err := client.ListTools(context.Background()); err != nil || len(tools) != 0 {
		t.Fatalf("identical repeated page did not terminate: %v %v", tools, err)
	}
}

func TestNameAndBinaryProjection(t *testing.T) {
	if got := PublicToolName("github", "search"); got != "mcp__github__search" {
		t.Fatal(got)
	}
	if PublicToolName("server", "a/b") == PublicToolName("server", "a_b") {
		t.Fatal("lossy normalization collision")
	}
	name := PublicToolName("server", strings.Repeat("a", 100))
	if len(name) != 64 {
		t.Fatal(len(name))
	}
	if !ValidServerName("my-server") || ValidServerName("a/b") {
		t.Fatal("server name validation")
	}
	text := RenderResourceResult("docs", map[string]any{"contents": []any{map[string]any{"blob": "c2VjcmV0", "text": "text stays"}}})
	if strings.Contains(text, "c2VjcmV0") || !strings.Contains(text, "text stays") || !strings.Contains(text, "8 base64") {
		t.Fatal(text)
	}
}
