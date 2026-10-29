package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"dsgo/internal/state"
)

func resourceDiscover() map[string]any {
	value := modernDiscover(false)
	value["capabilities"].(map[string]any)["resources"] = map[string]bool{}
	return value
}
func cacheNotification(c *Client, method string, params any) {
	_ = c.handle(mustJSON(wire{JSONRPC: "2.0", Method: method, Params: mustJSON(params)}), func(wire) error { return nil })
}

func TestMCPDefaultCacheAggregatesPagesAndInvalidatesResources(t *testing.T) {
	var lists, reads atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req wire
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "server/discover":
			mcpHTTPReply(w, req.ID, resourceDiscover())
		case "resources/list":
			lists.Add(1)
			var params map[string]string
			_ = json.Unmarshal(req.Params, &params)
			result := cachedResult(map[string]any{"resources": []any{map[string]string{"uri": "test://first", "name": "first"}}, "nextCursor": "next"})
			result["ttlMs"] = 60000
			if params["cursor"] == "next" {
				result = cachedResult(map[string]any{"resources": []any{map[string]string{"uri": "test://second", "name": "second"}}})
			}
			mcpHTTPReply(w, req.ID, result)
		case "resources/read":
			reads.Add(1)
			var params map[string]json.RawMessage
			_ = json.Unmarshal(req.Params, &params)
			var uri string
			_ = json.Unmarshal(params["uri"], &uri)
			result := cachedResult(map[string]any{"contents": []any{map[string]string{"uri": uri, "text": "body"}}})
			result["ttlMs"] = 60000
			mcpHTTPReply(w, req.ID, result)
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
	for n := 0; n < 2; n++ {
		rows, err := client.ListResources(context.Background())
		if err != nil || len(rows) != 2 {
			t.Fatalf("aggregate %v %v", rows, err)
		}
	}
	if lists.Load() != 2 {
		t.Fatalf("fresh aggregate not cached: %d", lists.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.ListResources(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cache swallowed cancelled caller: %v", err)
	}
	cacheNotification(client, "notifications/resources/list_changed", nil)
	if _, err := client.ListResources(context.Background()); err != nil {
		t.Fatal(err)
	}
	if lists.Load() != 4 {
		t.Fatal(lists.Load())
	}
	first, err := client.ReadResource(context.Background(), "test://first")
	if err != nil {
		t.Fatal(err)
	}
	first[0] = 'x'
	second, err := client.ReadResource(context.Background(), "test://first")
	if err != nil || !json.Valid(second) || reads.Load() != 1 {
		t.Fatalf("resource cache mutated or missed: %s %v calls=%d", second, err, reads.Load())
	}
	cacheNotification(client, "notifications/resources/updated", map[string]string{"uri": "test://other"})
	_, _ = client.ReadResource(context.Background(), "test://first")
	if reads.Load() != 1 {
		t.Fatal("unrelated URI invalidated resource")
	}
	cacheNotification(client, "notifications/resources/updated", map[string]string{"uri": "test://first"})
	_, _ = client.ReadResource(context.Background(), "test://first")
	if reads.Load() != 2 {
		t.Fatal("matching URI was not invalidated")
	}
}

func TestMCPMidReadInvalidationPreventsStaleCacheWrite(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req wire
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "server/discover" {
			mcpHTTPReply(w, req.ID, resourceDiscover())
			return
		}
		if req.Method == "resources/read" {
			if calls.Add(1) == 1 {
				close(entered)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			result := cachedResult(map[string]any{"contents": []any{map[string]string{"uri": "test://doc", "text": "body"}}})
			result["ttlMs"] = 60000
			mcpHTTPReply(w, req.ID, result)
			return
		}
		w.WriteHeader(400)
	}))
	defer server.Close()
	client, err := Open(context.Background(), t.TempDir(), state.MCPServer{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	done := make(chan error, 1)
	go func() { _, err := client.ReadResource(context.Background(), "test://doc"); done <- err }()
	<-entered
	cacheNotification(client, "notifications/resources/updated", map[string]string{"uri": "test://doc"})
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		if _, err := client.ReadResource(context.Background(), "test://doc"); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("invalidated in-flight value recached or fresh miss: %d", calls.Load())
	}
}

func TestMCPListPaginationHasExact64PageCap(t *testing.T) {
	var pages atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req wire
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "server/discover" {
			mcpHTTPReply(w, req.ID, resourceDiscover())
			return
		}
		n := pages.Add(1)
		mcpHTTPReply(w, req.ID, cachedResult(map[string]any{"resources": []any{map[string]string{"uri": fmt.Sprintf("test://%d", n), "name": "distinct"}}, "nextCursor": "same"}))
	}))
	defer server.Close()
	client, err := Open(context.Background(), t.TempDir(), state.MCPServer{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.ListResources(context.Background()); err == nil {
		t.Fatal("unbounded pagination succeeded")
	}
	if pages.Load() != 64 {
		t.Fatalf("page limit %d want64", pages.Load())
	}
	client.cache.mu.Lock()
	held := len(client.cache.entries)
	client.cache.mu.Unlock()
	if held != 0 {
		t.Fatal("partial aggregate cached")
	}
}

func TestMCPResponseCacheRetentionTTLBoundAndFIFO(t *testing.T) {
	now := time.Unix(1000, 0)
	cache := responseCache{now: func() time.Time { return now }}
	list := cacheKey{method: "tools/list"}
	stamp := cache.capture(list)
	cache.write(list, json.RawMessage(`{"tools":[],"ttlMs":0}`), stamp, true)
	if _, ok := cache.read(list); ok {
		t.Fatal("ttl0 served")
	}
	if _, ok := cache.entries[list]; !ok {
		t.Fatal("derived list source not retained")
	}
	resource := cacheKey{method: "resources/read", params: "zero"}
	cache.write(resource, json.RawMessage(`{"ttlMs":0}`), cache.capture(resource), false)
	if _, ok := cache.entries[resource]; ok {
		t.Fatal("ttl0 resource stored")
	}
	for n := 0; n < 513; n++ {
		key := cacheKey{method: "resources/read", params: fmt.Sprint(n)}
		cache.write(key, json.RawMessage(`{"ttlMs":999999999999}`), cache.capture(key), false)
	}
	if _, ok := cache.entries[cacheKey{method: "resources/read", params: "0"}]; ok {
		t.Fatal("oldest resource not evicted")
	}
	if _, ok := cache.entries[list]; !ok {
		t.Fatal("resource cap displaced tools list")
	}
	key := cacheKey{method: "resources/read", params: "512"}
	now = now.Add(24 * time.Hour)
	if _, ok := cache.read(key); ok {
		t.Fatal("TTL not clamped to24h")
	}
	old := cache.capture(list)
	cache.reset()
	cache.write(list, json.RawMessage(`{"ttlMs":1000}`), old, true)
	if len(cache.entries) != 0 {
		t.Fatal("closed-connection write repopulated cache")
	}
}

func TestMCPBadOutputSchemaFailsOnlyThatToolBeforeDispatch(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request wire
		_ = json.NewDecoder(r.Body).Decode(&request)
		switch request.Method {
		case "server/discover":
			mcpHTTPReply(w, request.ID, modernDiscover(false))
		case "tools/list":
			mcpHTTPReply(w, request.ID, cachedResult(map[string]any{"tools": []any{map[string]any{"name": "bad", "inputSchema": map[string]any{"type": "object"}, "outputSchema": map[string]any{"$ref": "https://dsgo.invalid/unavailable-schema"}}, supervisorTool("good")}}))
		case "tools/call":
			calls.Add(1)
			mcpHTTPReply(w, request.ID, completeResult(map[string]any{"content": []any{map[string]string{"type": "text", "text": "good"}}}))
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
	tools, err := client.ListTools(context.Background())
	if err != nil || len(tools) != 2 {
		t.Fatalf("bad output schema rejected whole generation: %v %v", tools, err)
	}
	if _, err := client.CallTool(context.Background(), "bad", json.RawMessage(`{}`)); err == nil {
		t.Fatal("uncompilable output schema accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("bad schema discovered after executing remote tool")
	}
	if _, err := client.CallTool(context.Background(), "good", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
}
