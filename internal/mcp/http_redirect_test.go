package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHTTPFetchRedirectMethodsAndBodies(t *testing.T) {
	for _, test := range []struct {
		code         int
		method, want string
		body         bool
	}{
		{301, "POST", "GET", false}, {302, "POST", "GET", false}, {303, "POST", "GET", false},
		{307, "POST", "POST", true}, {308, "POST", "POST", true},
		{302, "DELETE", "DELETE", true}, {303, "DELETE", "GET", false},
	} {
		t.Run(fmt.Sprintf("%d_%s", test.code, test.method), func(t *testing.T) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/start" {
					w.Header().Set("Location", "/end")
					w.WriteHeader(test.code)
					return
				}
				hits.Add(1)
				body, _ := io.ReadAll(r.Body)
				if r.Method != test.want || (string(body) == "payload") != test.body {
					t.Errorf("redirect = %s %q; want %s body=%v", r.Method, body, test.want, test.body)
				}
				for _, header := range []string{"Content-Type", "Content-Language", "Content-Encoding", "Content-Location"} {
					if !test.body && r.Header.Get(header) != "" {
						t.Errorf("body header %s survived GET conversion", header)
					}
				}
				if r.Header.Get("Authorization") != "Bearer configured" || r.Header.Get("Mcp-Method") != "tools/call" {
					t.Error("same-origin headers lost")
				}
				w.WriteHeader(204)
			}))
			defer server.Close()
			h, _ := newHTTP(server.URL+"/start", nil)
			req, _ := http.NewRequest(test.method, h.url, strings.NewReader("payload"))
			req.Header.Set("Authorization", "Bearer configured")
			req.Header.Set("Mcp-Method", "tools/call")
			for _, header := range []string{"Content-Type", "Content-Language", "Content-Encoding", "Content-Location"} {
				req.Header.Set(header, "configured")
			}
			res, err := h.fetch(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if hits.Load() != 1 {
				t.Fatalf("destination calls %d", hits.Load())
			}
		})
	}
}

func TestHTTPFetchRedirectOriginStrippingAndNoCredentialRestoration(t *testing.T) {
	var original *httptest.Server
	var destinationHits, returnHits atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationHits.Add(1)
		for _, header := range []string{"Authorization", "Proxy-Authorization", "Cookie"} {
			if r.Header.Get(header) != "" {
				t.Errorf("cross-port %s leaked", header)
			}
		}
		if r.Host == "configured.example" || r.Header.Get("X-Api-Key") != "custom" || r.Header.Get("Mcp-Param-Tenant") != "tenant" {
			t.Error("cross-origin Host/custom header rules differed")
		}
		w.Header().Set("Location", original.URL+"/return")
		w.WriteHeader(307)
	}))
	defer destination.Close()
	original = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/return" {
			returnHits.Add(1)
			if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				t.Error("original credentials restored after cross-origin hop")
			}
			w.WriteHeader(204)
			return
		}
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(308)
	}))
	defer original.Close()
	h, _ := newHTTP(original.URL, map[string]string{"Authorization": "Bearer configured", "Proxy-Authorization": "proxy", "Cookie": "secret", "Host": "configured.example", "X-Api-Key": "custom", "Mcp-Param-Tenant": "tenant"})
	res, err := h.request(context.Background(), "POST", []byte(`{}`), ProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if destinationHits.Load() != 1 || returnHits.Load() != 1 {
		t.Fatalf("redirect counts %d %d", destinationHits.Load(), returnHits.Load())
	}
}

func TestHTTPFetchRedirectTwentyHopBoundaryAndProtocol(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if n >= limit {
			w.WriteHeader(204)
			return
		}
		w.Header().Set("Location", fmt.Sprintf("/?n=%d&limit=%d", n+1, limit))
		w.WriteHeader(307)
	}))
	defer server.Close()
	for _, limit := range []int{20, 21} {
		hits.Store(0)
		h, _ := newHTTP(fmt.Sprintf("%s/?limit=%d", server.URL, limit), nil)
		res, err := h.request(context.Background(), "POST", []byte(`{}`), ProtocolVersion)
		if limit == 20 {
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
		} else if err == nil {
			res.Body.Close()
			t.Fatal("twenty-first redirect followed")
		}
		if hits.Load() != 21 {
			t.Fatalf("hits %d for limit %d", hits.Load(), limit)
		}
	}
	for _, pair := range []struct {
		a, b string
		same bool
	}{
		{"http://EXAMPLE.test/a", "http://example.test:80/b", true},
		{"https://example.test", "https://example.test:443", true},
		{"http://example.test", "https://example.test", false},
		{"http://example.test:8000", "http://example.test:8001", false},
		{"https://example.test", "https://sub.example.test", false},
		{"https://例子.测试", "https://xn--fsqu00a.xn--0zwm56d", true},
	} {
		a, _ := url.Parse(pair.a)
		b, _ := url.Parse(pair.b)
		if sameHTTPOrigin(a, b) != pair.same {
			t.Errorf("origin %s %s", pair.a, pair.b)
		}
	}
}

func TestHTTPJSONAndSSEDoNotInheritStdioBufferLimit(t *testing.T) {
	large := strings.Repeat("x", stdioReadBufferBytes+1)
	for _, sse := range []bool{false, true} {
		t.Run(strconv.FormatBool(sse), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req wire
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				if !strings.Contains(string(req.Params), large) {
					t.Error("outbound HTTP body truncated")
				}
				answer, _ := json.Marshal(wire{JSONRPC: "2.0", ID: req.ID, Result: mustJSON(map[string]string{"text": large})})
				if sse {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: %s\n\n", answer)
				} else {
					w.Header().Set("Content-Type", "application/json")
					w.Write(answer)
				}
			}))
			defer server.Close()
			h, _ := newHTTP(server.URL, nil)
			c := &Client{ProtocolVersion: LegacyProtocolVersion, http: h, pending: map[string]chan response{}}
			var out struct{ Text string }
			err := c.httpCall(context.Background(), wire{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call", Params: mustJSON(map[string]string{"text": large})}, &out)
			if err != nil || out.Text != large {
				t.Fatalf("large HTTP result bytes=%d err=%v", len(out.Text), err)
			}
		})
	}
}
