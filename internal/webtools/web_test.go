package webtools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"dsgo/internal/core"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(r *http.Request, status int, kind, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{kind}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func TestMessagesSearchWireStructuredResultsAndFanout(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	events := 0
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://api.deepseek.com/anthropic/v1/messages" {
			t.Errorf("wrong search URL %s", r.URL)
		}
		if r.Header.Get("x-api-key") != "key" || r.Header.Get("Authorization") != "Bearer key" || r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Error("wrong search auth")
		}
		var p struct {
			Model     string            `json:"model"`
			MaxTokens int               `json:"max_tokens"`
			Messages  []core.Message    `json:"messages"`
			Tools     []map[string]any  `json:"tools"`
			Thinking  map[string]string `json:"thinking"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		if p.Model != "deepseek-v4-flash" || p.MaxTokens != 4096 || p.Thinking["type"] != "disabled" || p.Tools[0]["type"] != "web_search_20250305" || p.Tools[0]["max_uses"] != float64(5) {
			t.Errorf("wrong search payload %+v", p)
		}
		query := strings.TrimPrefix(p.Messages[0].Content[0].Text, "Perform a web search for the query: ")
		mu.Lock()
		calls++
		mu.Unlock()
		if query == "first" {
			return response(r, 200, "application/json", `{"content":[{"type":"text","text":"ignore https://prose.invalid","citations":[{"url":"https://a.test","cited_text":"excerpt"}]},{"type":"web_search_tool_result","content":[{"type":"web_search_result","url":"https://a.test","title":"A","page_age":"2026-10-01"},{"type":"web_search_result","url":"https://c.test","title":"C"}]}]}`), nil
		}
		return response(r, 200, "application/json", `{"content":[{"type":"web_search_tool_result","content":[{"type":"web_search_result","url":"https://b.test","title":"B"},{"type":"web_search_result","url":"https://a.test","title":"duplicate"}]}]}`), nil
	})}
	e := New(Options{APIKey: "key", HTTPClient: client})
	result, err := e.Execute(context.Background(), core.ToolContext{Emit: func(name string, data any) error {
		if name == "web/deepseek-search-llm-request" {
			mu.Lock()
			events++
			mu.Unlock()
			raw := string(core.JSON(data))
			if strings.Contains(raw, `"key"`) {
				t.Error("credential in event")
			}
		}
		return nil
	}}, "web_search", json.RawMessage(`{"queries":["first","second","first"]}`))
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 || events != 2 {
		t.Fatalf("dedup/request logging %d %d", calls, events)
	}
	if strings.Contains(result.Content, "prose.invalid") || !strings.Contains(result.Content, "excerpt") || strings.Count(result.Content, "https://a.test") != 1 {
		t.Fatal(result.Content)
	}
	if !(strings.Index(result.Content, "https://a.test") < strings.Index(result.Content, "https://b.test") && strings.Index(result.Content, "https://b.test") < strings.Index(result.Content, "https://c.test")) {
		t.Fatal("not round-robin merge")
	}
}

func TestDirectSearchAndFetchWire(t *testing.T) {
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer key" || r.Header.Get("x-api-key") != "" {
			t.Error("wrong direct API auth")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/v1/tools/search":
			if body["count"] != float64(8) || body["need_content"] != false || body["ori_question"] != "what is new?" {
				t.Errorf("wrong direct body %+v", body)
			}
			return response(r, 200, "application/json", `{"results":[{"url":"https://a.test","title":"A","snippet":"snippet","date":null}]}`), nil
		case "/v1/tools/fetch":
			if body["url"] != "https://a.test" {
				t.Error("wrong fetch target")
			}
			return response(r, 200, "application/json", `{"content":"# Markdown page"}`), nil
		default:
			t.Errorf("wrong API path %s", r.URL.Path)
			return response(r, 404, "application/json", `{}`), nil
		}
	})}
	e := New(Options{APIKey: "key", UseDirectAPI: true, HTTPClient: client, OriginalQuestion: func(string) string { return "  what\n is   new? " }})
	if _, err := e.Execute(context.Background(), core.ToolContext{}, "web_search", json.RawMessage(`{"queries":["query"]}`)); err != nil {
		t.Fatal(err)
	}
	result, err := e.Execute(context.Background(), core.ToolContext{}, "web_fetch", json.RawMessage(`{"url":"https://a.test"}`))
	if err != nil || !strings.Contains(result.Content, "Fetched https://a.test (HTTP 200)") || !strings.Contains(result.Content, "# Markdown page") {
		t.Fatalf("%s %v", result.Content, err)
	}
}

func TestCredentialsOriginRedirectAndMissingBlocks(t *testing.T) {
	t.Run("durable request failure prevents dispatch", func(t *testing.T) {
		called := false
		storageError := errors.New("disk full")
		e := New(Options{APIKey: "key", HTTPClient: &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
			called = true
			return nil, errors.New("should not dispatch")
		})}})
		_, err := e.Execute(context.Background(), core.ToolContext{Emit: func(string, any) error { return storageError }}, "web_search", json.RawMessage(`{"queries":["q"]}`))
		if !errors.Is(err, storageError) || called {
			t.Fatalf("request escaped failed persistence: %v %v", err, called)
		}
	})
	t.Run("account precedence", func(t *testing.T) {
		e := New(Options{APIKey: "key", Token: "account", HTTPClient: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("x-dsh-auth-token") != "account" || r.Header.Get("Authorization") != "" || r.Header.Get("x-api-key") != "" {
				t.Error("mixed account and key")
			}
			return response(r, 200, "application/json", `{"content":[{"type":"web_search_tool_result","content":[]}]}`), nil
		})}})
		if _, err := e.Execute(context.Background(), core.ToolContext{}, "web_search", json.RawMessage(`{"queries":["q"]}`)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("account cannot escape origin", func(t *testing.T) {
		called := false
		e := New(Options{Token: "account", SearchBaseURL: "https://other.example/anthropic/v1", HTTPClient: &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) { called = true; return nil, errors.New("should not call") })}})
		events := 0
		_, err := e.Execute(context.Background(), core.ToolContext{Emit: func(string, any) error { events++; return nil }}, "web_search", json.RawMessage(`{"queries":["q"]}`))
		if err == nil || called || events != 0 {
			t.Fatalf("credential escaped or pre-dispatch event recorded: %v %v %d", err, called, events)
		}
	})
	t.Run("redirect rejected", func(t *testing.T) {
		calls := 0
		e := New(Options{APIKey: "key", HTTPClient: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			calls++
			res := response(r, 302, "application/json", `{}`)
			res.Header.Set("Location", "https://elsewhere.example")
			return res, nil
		})}})
		if _, err := e.Execute(context.Background(), core.ToolContext{}, "web_search", json.RawMessage(`{"queries":["q"]}`)); err == nil || calls != 1 {
			t.Fatalf("followed API redirect %d %v", calls, err)
		}
	})
	t.Run("no prose fallback", func(t *testing.T) {
		e := New(Options{APIKey: "key", HTTPClient: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			return response(r, 200, "application/json", `{"content":[{"type":"text","text":"https://somewhere.example"}]}`), nil
		})}})
		if _, err := e.Execute(context.Background(), core.ToolContext{}, "web_search", json.RawMessage(`{"queries":["q"]}`)); err == nil {
			t.Fatal("accepted response without structured search")
		}
	})
}

func TestFetchTransportPolicyAndCaps(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "100.64.1.1", "169.254.169.254", "198.18.1.1", "192.0.2.1", "::1", "fc00::1", "fe80::1", "::ffff:127.0.0.1", "2001:db8::1", "2002:0808:0808::"} {
		if publicIP(netip.MustParseAddr(ip)) {
			t.Errorf("accepted non-public %s", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !publicIP(netip.MustParseAddr(ip)) {
			t.Errorf("rejected public %s", ip)
		}
	}
	for _, target := range []string{"file:///etc/passwd", "https://user:password@example.com", "http://[fe80::1%25en0]/"} {
		if _, err := validateURL(target); err == nil {
			t.Errorf("accepted URL %s", target)
		}
	}
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("anonymous fetch carried credentials")
		}
		return response(r, 404, "text/plain; charset=utf-8", strings.Repeat("界", 100001)), nil
	})}
	result, err := fetchWithClient(context.Background(), "https://example.com/missing", client)
	if err != nil {
		t.Fatal(err)
	}
	if result.StatusCode != 404 || !result.Truncated || len([]rune(result.Body)) != 100000 {
		t.Fatalf("bad bounded non-2xx result %+v", result)
	}
	calls := 0
	client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		res := response(r, 302, "text/plain", "")
		res.Header.Set("Location", "https://other.example/path")
		return res, nil
	})
	if _, err := fetchWithClient(context.Background(), "https://example.com", client); err == nil || calls != 1 {
		t.Fatalf("cross-origin redirect followed: %v %d", err, calls)
	}
}

func TestHTMLFilteringGFMAndDepth(t *testing.T) {
	body := `<html><head><script>bad head</script></head><body><h1>Title</h1><p>Good <strong>bold</strong> <del>old</del> <a href="/doc">link</a>.</p><script>alert('evil')</script><style>.fake{}</style><div hidden>hidden one</div><p aria-hidden="true">hidden two</p><div style="display: none">hidden three</div><a href="javascript:alert(1)">plain text</a><table><tr><th>A</th><th>B</th></tr><tr><td>1</td><td>2</td></tr></table><pre><code>let x = 1;
  x++;</code></pre></body></html>`
	got := htmlMarkdown(body, "https://example.com/base")
	for _, bad := range []string{"bad head", "evil", "hidden one", "hidden two", "hidden three", "javascript:", ".fake"} {
		if strings.Contains(got, bad) {
			t.Errorf("HTML leaked %s in %s", bad, got)
		}
	}
	for _, want := range []string{"# Title", "**bold**", "~~old~~", "[link](https://example.com/doc)", "| A | B |", "| --- | --- |", "  x++;"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if got := htmlMarkdown(strings.Repeat("<div>", 513)+"deep", ""); got != htmlOmitted {
		t.Fatalf("deep HTML was converted: %s", bounded(got, 100))
	}
}
