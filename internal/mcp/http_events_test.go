package mcp

import (
	"context"
	"encoding/json"
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

func TestHTTPPostStreamResumesWithoutRepeatingToolCall(t *testing.T) {
	var calls, resumes atomic.Int64
	var mu sync.Mutex
	var requestID json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(204)
			return
		}
		if r.Method == http.MethodGet {
			if r.Header.Get("Last-Event-ID") != "cursor-1" {
				w.WriteHeader(405)
				return
			}
			resumes.Add(1)
			mu.Lock()
			id := string(requestID)
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "id: cursor-2\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"resumed\"}]}}\n\n", id)
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
			supervisorInitialize(w, req, 1, "")
		case "notifications/initialized":
			w.WriteHeader(202)
		case "tools/call":
			calls.Add(1)
			mu.Lock()
			requestID = append(json.RawMessage(nil), req.ID...)
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "id: cursor-1\nretry: 1\ndata:\n\n")
		}
	}))
	defer server.Close()
	client, err := Open(context.Background(), t.TempDir(), state.MCPServer{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	result, err := client.CallTool(context.Background(), "mutate", json.RawMessage(`{}`))
	if err != nil || len(result.Content) != 1 || !strings.Contains(string(result.Content[0]), "resumed") {
		t.Fatalf("resumed response: %+v %v", result, err)
	}
	if calls.Load() != 1 || resumes.Load() != 1 {
		t.Fatalf("POST count %d, GET resume count %d", calls.Load(), resumes.Load())
	}
}

func TestHTTPNotificationStreamResumesCursorAndCloses(t *testing.T) {
	var gets atomic.Int64
	resumed := make(chan struct{})
	closed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(204)
			return
		}
		if r.Method == http.MethodGet {
			n := gets.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			if n == 1 {
				fmt.Fprint(w, "id: notification-1\nretry: 1\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/list_changed\"}\n\n")
				return
			}
			if r.Header.Get("Last-Event-ID") != "notification-1" {
				t.Errorf("lost cursor %q", r.Header.Get("Last-Event-ID"))
			}
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			close(resumed)
			<-r.Context().Done()
			close(closed)
			return
		}
		var req wire
		_ = json.NewDecoder(r.Body).Decode(&req)
		if legacyProbeReply(w, req) {
			return
		}
		if req.Method == "initialize" {
			supervisorInitialize(w, req, 1, "")
			return
		}
		w.WriteHeader(202)
	}))
	defer server.Close()
	client, err := Open(context.Background(), t.TempDir(), state.MCPServer{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	select {
	case <-resumed:
	case <-time.After(time.Second):
		t.Fatal("GET stream did not resume")
	}
	select {
	case <-client.toolsChanged:
	case <-time.After(time.Second):
		t.Fatal("list change was lost")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("GET stream survived close")
	}
}

// Golden behavior from the fixed lockfile's eventsource-parser@3.1.0, whose
// stream wrapper uses no maxBufferSize and no EOF flush/consume hook.
func TestSSEParserDataDispatchRetryAndEOF(t *testing.T) {
	for _, test := range []struct {
		name, input    string
		count          int
		data, id, kind string
		retries        int
	}{
		{"id-only", "id: cursor\n\n", 0, "", "", "", 0},
		{"retry-only", "retry: 0\n\n", 0, "", "", "", 1},
		{"retry-before-eof", "retry: 0\n", 0, "", "", "", 1},
		{"priming-empty-data", "id: cursor\ndata:\n\n", 1, "", "cursor", "", 0},
		{"incomplete-eof", "id: cursor\ndata: lost\n", 0, "", "", "", 0},
		{"incomplete-data-line", "data: lost", 0, "", "", "", 0},
		{"trailing-cr", "data: lost\r\r", 0, "", "", "", 0},
		{"crlf", "\xef\xbb\xbfdata: one\r\ndata: two\r\n\r\n", 1, "one\ntwo", "", "", 0},
		{"bare-cr", "data: done\r\r: keep-alive\n", 1, "done", "", "", 0},
		{"typed-event", "event: custom\nid: cursor\ndata: payload\n\n", 1, "payload", "cursor", "custom", 0},
		{"nul-resets-id", "id: earlier\nid: invalid\x00id\ndata: done\n\n", 1, "done", "", "", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got []sseEvent
			retries := 0
			err := parseSSEEvents(strings.NewReader(test.input), func(event sseEvent) error {
				if event.hasRetry {
					retries++
					if event.retry != 0 {
						t.Errorf("retry %s", event.retry)
					}
					return nil
				}
				got = append(got, event)
				return nil
			})
			if err != nil || len(got) != test.count || retries != test.retries {
				t.Fatalf("events %d retries %d error %v", len(got), retries, err)
			}
			if len(got) > 0 {
				id := ""
				if got[0].hasID {
					id = got[0].id
				}
				if string(got[0].data) != test.data || id != test.id || got[0].kind != test.kind {
					t.Fatalf("event %+v", got[0])
				}
			}
		})
	}
}

func TestHTTPStreamCancellationDoesNotResume(t *testing.T) {
	var resumes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(204)
			return
		}
		if r.Method == http.MethodGet {
			if r.Header.Get("Last-Event-ID") != "" {
				resumes.Add(1)
			}
			w.WriteHeader(405)
			return
		}
		var req wire
		_ = json.NewDecoder(r.Body).Decode(&req)
		if legacyProbeReply(w, req) {
			return
		}
		switch req.Method {
		case "initialize":
			supervisorInitialize(w, req, 1, "")
		case "tools/call":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "id: pending\nretry: 100000\n\n")
		default:
			w.WriteHeader(202)
		}
	}))
	defer server.Close()
	client, err := Open(context.Background(), t.TempDir(), state.MCPServer{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := client.CallTool(ctx, "mutate", json.RawMessage(`{}`)); err == nil {
		t.Fatal("cancelled stream succeeded")
	}
	if resumes.Load() != 0 {
		t.Fatal("cancelled request resumed")
	}
}
