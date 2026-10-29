package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var ErrSessionExpired = errors.New("MCP HTTP session expired; reconnect before the next call")
var ErrHTTPTransport = errors.New("MCP HTTP transport request failed")

type HTTPStatusError struct {
	StatusCode int
	Body       json.RawMessage
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("MCP HTTP server returned status %d", e.StatusCode)
}

type httpTransport struct {
	url       string
	headers   map[string]string
	client    *http.Client
	mu        sync.Mutex
	sessionID string
	protocol  string
}

func newHTTP(endpoint string, headers map[string]string) (*httpTransport, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" {
		return nil, errors.New("MCP URL must be http(s), without embedded credentials or fragment")
	}
	copyHeaders := map[string]string{}
	for key, value := range headers {
		copyHeaders[key] = value
	}
	return &httpTransport{url: endpoint, headers: copyHeaders, client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (h *httpTransport) request(ctx context.Context, method string, message []byte, protocol string) (*http.Response, error) {
	return h.requestWithResume(ctx, method, message, protocol, "")
}

func (h *httpTransport) requestWithResume(ctx context.Context, method string, message []byte, protocol, lastEventID string, extra ...map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, h.url, bytes.NewReader(message))
	if err != nil {
		return nil, err
	}
	for key, value := range h.headers {
		req.Header.Set(key, value)
		if strings.EqualFold(key, "Host") {
			req.Host = value
		}
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if method == http.MethodGet {
		req.Header.Set("Accept", "text/event-stream")
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	if protocol != "" {
		req.Header.Set("MCP-Protocol-Version", protocol)
	}
	applyModernHeaders(req, message)
	for _, headers := range extra {
		for name, value := range headers {
			if strings.HasPrefix(strings.ToLower(name), "mcp-param-") {
				req.Header.Set(name, value)
			}
		}
	}
	h.mu.Lock()
	session := h.sessionID
	h.mu.Unlock()
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	var outbound wire
	if len(message) > 0 && json.Unmarshal(message, &outbound) == nil && outbound.Method == "initialize" {
		req.Header.Del("Mcp-Session-Id")
	}
	res, err := h.fetch(req)
	if err != nil {
		return nil, ErrHTTPTransport
	}
	if res.StatusCode == http.StatusNotFound && session != "" {
		res.Body.Close()
		return nil, ErrSessionExpired
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return nil, &HTTPStatusError{StatusCode: res.StatusCode, Body: body}
	}
	return res, nil
}

func (c *Client) httpCall(ctx context.Context, message wire, out any) error {
	id := responseKey(message.ID)
	ch := make(chan response, 1)
	c.mu.Lock()
	if c.terminalError != nil {
		c.mu.Unlock()
		return ErrClosed
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	b, err := json.Marshal(message)
	if err != nil {
		return err
	}
	headers, _ := ctx.Value(paramHeadersContextKey{}).(map[string]string)
	res, err := c.http.requestWithResume(ctx, http.MethodPost, b, c.ProtocolVersion, "", headers)
	if err != nil {
		if ctx.Err() != nil {
			c.cancelHTTP(message)
			return ctx.Err()
		}
		if c.modern() {
			if rpc := matchingHTTPError(err, message); rpc != nil {
				return rpc
			}
		}
		return err
	}
	defer res.Body.Close()
	if message.Method == "initialize" {
		session := res.Header.Get("Mcp-Session-Id")
		if session != "" {
			for _, r := range session {
				if r < 0x21 || r > 0x7e {
					return errors.New("MCP returned invalid HTTP session ID")
				}
			}
		}
		c.http.mu.Lock()
		c.http.sessionID = session
		c.http.mu.Unlock()
	}
	typeName, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
	reply := func(answer wire) error { return c.httpNotify(ctx, answer) }
	switch typeName {
	case "application/json":
		b, readErr := io.ReadAll(res.Body)
		if readErr != nil {
			err = readErr
			break
		}
		err = c.handle(b, reply)
	case "text/event-stream":
		err = c.readHTTPReplyStream(ctx, res, ch, reply)
	default:
		return fmt.Errorf("unsupported MCP HTTP content type %q", typeName)
	}
	if err != nil {
		if ctx.Err() != nil {
			c.cancelHTTP(message)
			return ctx.Err()
		}
		return err
	}
	select {
	case answer := <-ch:
		if answer.Error != nil {
			return answer.Error
		}
		if out != nil {
			return json.Unmarshal(answer.Result, out)
		}
		return nil
	default:
		if ctx.Err() != nil {
			c.cancelHTTP(message)
			return ctx.Err()
		}
		return errors.New("MCP HTTP stream ended without matching response")
	}
}

func (c *Client) cancelHTTP(message wire) {
	if message.Method == "initialize" || message.Method == "server/discover" || c.modern() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_ = c.notify(ctx, "notifications/cancelled", map[string]any{"requestId": message.ID, "reason": "request cancelled"})
}

func (c *Client) httpNotify(ctx context.Context, message wire) error {
	b, err := json.Marshal(message)
	if err != nil {
		return err
	}
	res, err := c.http.request(ctx, http.MethodPost, b, c.ProtocolVersion)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusAccepted && res.StatusCode != http.StatusNoContent {
		return fmt.Errorf("MCP HTTP notification returned status %d, expected 202", res.StatusCode)
	}
	return nil
}

func (h *httpTransport) close() error {
	h.mu.Lock()
	session := h.sessionID
	protocol := h.protocol
	h.mu.Unlock()
	if session == "" {
		h.client.CloseIdleConnections()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// DELETE may return 405 when explicit session termination is unsupported.
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, h.url, nil)
	if err != nil {
		return err
	}
	for key, value := range h.headers {
		req.Header.Set(key, value)
	}
	req.Header.Set("Mcp-Session-Id", session)
	if protocol == "" {
		protocol = ProtocolVersion
	}
	req.Header.Set("MCP-Protocol-Version", protocol)
	res, err := h.client.Do(req)
	h.client.CloseIdleConnections()
	if err != nil {
		return errors.New("MCP HTTP session close failed")
	}
	defer res.Body.Close()
	if (res.StatusCode >= 200 && res.StatusCode < 300) || res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusMethodNotAllowed {
		return nil
	}
	return fmt.Errorf("MCP HTTP session close returned status %d", res.StatusCode)
}

// RenderResourceResult keeps canonical binary payloads out of the model context.
// Programmatic callers can retain the original value returned by Call instead.
func RenderResourceResult(server string, value any) string {
	b, _ := json.Marshal(value)
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return "MCP server: " + server + "\n[invalid resource result]"
	}
	var sanitize func(any) any
	sanitize = func(item any) any {
		switch x := item.(type) {
		case map[string]any:
			for key, value := range x {
				if key == "blob" {
					if s, ok := value.(string); ok {
						x[key] = fmt.Sprintf("[binary resource: %d base64 characters; available to programmatic callers]", len(s))
						continue
					}
				}
				x[key] = sanitize(value)
			}
		case []any:
			for i, value := range x {
				x[i] = sanitize(value)
			}
		}
		return item
	}
	b, _ = json.Marshal(sanitize(v))
	return "MCP server: " + strings.TrimSpace(server) + "\n" + string(b)
}
