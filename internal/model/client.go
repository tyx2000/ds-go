// Package model implements the fixed DeepSeek Messages transport used by dsgo.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"dsgo/internal/core"
)

const DefaultBaseURL = "https://api.deepseek.com/anthropic"
const filesBeta = "files-api-2025-04-14"

type Options struct {
	BaseURL string
	APIKey  string
	// Token is an already acquired DeepSeek account grant, not a web cookie.
	Token      string
	HTTPClient *http.Client
	// MaxRetries is the number of retries before any response content is received.
	// Zero uses two retries; negative disables retries.
	MaxRetries  int
	RetryDelay  time.Duration
	IdleTimeout time.Duration
	// DisableFiles sends prepared base64 images inline instead of uploading them.
	DisableFiles bool
	// ReplayKey scopes opaque replay to the endpoint and effective credentials.
	// A stable private 32-byte key permits replay after restarting the CLI. An
	// omitted key creates an instance-local key and safely degrades old replay.
	ReplayKey []byte `json:"-"`
}

type Client struct {
	root                    string
	apiKey, token           string
	http                    *http.Client
	initErr                 error
	retries                 int
	retryDelay, idleTimeout time.Duration
	disableFiles            bool
	mu                      sync.Mutex
	files                   map[string]cachedFile
	replayKey               []byte
}

var _ core.Model = (*Client)(nil)

// Error carries stable failure metadata. It deliberately never retains response
// bodies or request URLs, which a gateway may fill with credentials or prompts.
type Error struct {
	Code       string
	Status     int
	RequestID  string
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("DeepSeek Messages: %s (HTTP %d)", e.Code, e.Status)
	}
	return "DeepSeek Messages: " + e.Code
}

func New(options Options) *Client {
	c := &Client{apiKey: options.APIKey, token: options.Token, retries: options.MaxRetries,
		retryDelay: options.RetryDelay, idleTimeout: options.IdleTimeout,
		disableFiles: options.DisableFiles, files: make(map[string]cachedFile)}
	var replayErr error
	c.replayKey, replayErr = replayKey(options.ReplayKey)
	if c.retries == 0 {
		c.retries = 2
	}
	if c.retries < 0 {
		c.retries = 0
	}
	if c.retries > 5 {
		c.retries = 5
	}
	if c.retryDelay <= 0 {
		c.retryDelay = 250 * time.Millisecond
	}
	if c.idleTimeout <= 0 {
		c.idleTimeout = 5 * time.Minute
	}
	base := options.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimSpace(base) != base {
		c.initErr = &Error{Code: "INVALID_BASE_URL"}
	} else {
		c.root = strings.TrimRight(base, "/")
		if !strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/v1") {
			c.root += "/v1"
		}
	}
	if c.apiKey != "" && c.token != "" {
		c.initErr = &Error{Code: "AMBIGUOUS_CREDENTIAL"}
	}
	for _, value := range []string{c.apiKey, c.token} {
		if strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
			c.initErr = &Error{Code: "INVALID_CREDENTIAL"}
		}
	}
	if replayErr != nil {
		c.initErr = replayErr
	}
	if options.HTTPClient == nil {
		c.http = &http.Client{}
	} else {
		copy := *options.HTTPClient
		c.http = &copy
	}
	// Even an injected client cannot follow a redirect with authentication headers.
	c.http.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

func (c *Client) ready() error {
	if c.initErr != nil {
		return c.initErr
	}
	if c.apiKey == "" && c.token == "" {
		return &Error{Code: "MISSING_CREDENTIAL"}
	}
	return nil
}

func (c *Client) request(ctx context.Context, method, endpoint string, body io.Reader, contentType string, files bool, extra ...http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.root+endpoint, body)
	if err != nil {
		return nil, &Error{Code: "INVALID_REQUEST"}
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "text/event-stream, application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("User-Agent", "dsgo/0.1 (DeepSeek Messages terminal client)")
	if c.token != "" {
		req.Header.Set("x-dsh-auth-token", c.token)
	} else {
		req.Header.Set("x-api-key", c.apiKey)
	}
	if files {
		req.Header.Set("Anthropic-Beta", filesBeta)
	}
	for _, headers := range extra {
		for key, values := range headers {
			req.Header[key] = values
		}
	}
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{Code: "TRANSPORT"}
	}
	return response, nil
}

// Stream returns all received blocks even on cancellation or a truncated stream.
// Delta types are text, thinking, signature, tool_input, block_start, block_stop.
// No retry occurs once a content block has started, even if its text is empty.
func (c *Client) Stream(ctx context.Context, input core.Request, emit func(core.Delta)) (core.Response, error) {
	if input.Provider == "" {
		input.Provider = "deepseek-official"
	}
	if err := c.ready(); err != nil {
		return core.Response{}, err
	}
	if ctx.Err() != nil {
		return core.Response{}, ctx.Err()
	}
	input.ReplayIdentity = c.replayIdentity(input.Provider)
	body, err := serialize(input)
	if err != nil {
		return core.Response{}, err
	}
	inline := cloneWireRequest(body)
	hasFiles, uploaded, err := c.prepareImages(ctx, &body)
	if err != nil {
		return core.Response{}, err
	}
	if !hasFiles && imageBytes(body) > maxInlineImageBytes {
		return core.Response{}, &Error{Code: "IMAGE_LIMIT"}
	}
	if emit == nil {
		emit = func(core.Delta) {}
	}
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return core.Response{}, ctx.Err()
		}
		payload, err := json.Marshal(body)
		if err != nil || len(payload) > maxRequestBytes {
			return core.Response{}, &Error{Code: "REQUEST_LIMIT"}
		}
		callCtx, cancel := context.WithCancel(ctx)
		watch := newIdleWatch(cancel, c.idleTimeout)
		headers := make(http.Header)
		if input.UserID != "" {
			headers.Set("x-deepseek-harness-user-id", input.UserID)
		}
		if input.SessionID != "" {
			headers.Set("x-deepseek-harness-session-id", input.SessionID)
		}
		if input.Purpose == "compaction" {
			headers.Set("x-deepseek-harness-compact", "1")
		}
		var betas []string
		if hasFiles {
			betas = append(betas, filesBeta)
		}
		for _, message := range body.Messages {
			for _, b := range message.Content {
				if b.Type == "tool_addition" || b.Type == "tool_removal" {
					betas = append(betas, "mid-conversation-tool-changes-2026-07-01")
					break
				}
			}
			if len(betas) > 0 && betas[len(betas)-1] == "mid-conversation-tool-changes-2026-07-01" {
				break
			}
		}
		if len(betas) > 0 {
			headers.Set("Anthropic-Beta", strings.Join(betas, ","))
		}
		raw, err := c.request(callCtx, http.MethodPost, "/messages", bytes.NewReader(payload), "application/json", hasFiles, headers)
		var result core.Response
		started := false
		var retryAfter time.Duration
		if err == nil {
			if raw.StatusCode < 200 || raw.StatusCode >= 300 {
				err = c.responseError(raw)
				if failure, ok := err.(*Error); ok {
					retryAfter = failure.RetryAfter
				}
			} else {
				reader := &activityReader{reader: io.LimitReader(raw.Body, maxResponseBytes+1), watch: watch}
				if strings.HasPrefix(strings.ToLower(raw.Header.Get("Content-Type")), "application/json") {
					result, started, err = parseJSONResponse(reader, emit)
				} else {
					result, started, err = parseSSE(reader, emit)
				}
			}
			raw.Body.Close()
		}
		timedOut := watch.stop()
		cancel()
		result.ReplayIdentity = input.ReplayIdentity
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if timedOut {
			err = &Error{Code: "TIMEOUT"}
		}
		if err == nil {
			return result, nil
		}
		// A stale uploaded reference can be recovered only before any output. All
		// prepared images fall back together; an individual request never mixes
		// newly uploaded references and inline data.
		var failure *Error
		if uploaded && !started && errors.As(err, &failure) && failure.Code == "STALE_FILE" {
			c.mu.Lock()
			clear(c.files)
			c.mu.Unlock()
			if imageBytes(inline) > maxInlineImageBytes {
				return result, &Error{Code: "IMAGE_LIMIT"}
			}
			body, hasFiles, uploaded = inline, containsFile(inline), false
			continue
		}
		if started || attempt >= c.retries || !retryable(err) {
			return result, err
		}
		delay := c.retryDelay * time.Duration(1<<min(attempt, 4))
		if retryAfter > delay {
			delay = retryAfter
		}
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, ctx.Err()
		case <-timer.C:
		}
	}
}

func retryable(err error) bool {
	var failure *Error
	if !errors.As(err, &failure) {
		return false
	}
	switch failure.Code {
	case "RATE_LIMIT", "SERVER", "TRANSPORT", "TIMEOUT", "STREAM_CLOSED", "EMPTY_RESPONSE":
		return true
	}
	return false
}

func (c *Client) responseError(response *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	var envelope struct {
		Error struct{ Type, Code, Message string }
	}
	_ = json.Unmarshal(data, &envelope)
	detail := strings.ToLower(envelope.Error.Type + " " + envelope.Error.Code + " " + envelope.Error.Message)
	code := "HTTP_" + strconv.Itoa(response.StatusCode)
	switch {
	case response.StatusCode == 401 || response.StatusCode == 403:
		code = "AUTH"
	case response.StatusCode == 402 || strings.Contains(detail, "quota") || strings.Contains(detail, "insufficient_balance"):
		code = "QUOTA"
	case response.StatusCode == 429:
		code = "RATE_LIMIT"
	case core.IsContextWindowExceeded(detail):
		code = "CONTEXT_WINDOW_EXCEEDED"
	case response.StatusCode >= 500:
		code = "SERVER"
	case response.StatusCode == 400 || response.StatusCode == 413:
		code = "INVALID_REQUEST"
	}
	if code == "INVALID_REQUEST" && response.StatusCode == 400 && strings.Contains(detail, "file") && (strings.Contains(detail, "not found") || strings.Contains(detail, "expired") || strings.Contains(detail, "invalid file")) {
		code = "STALE_FILE"
	}
	failure := &Error{Code: code, Status: response.StatusCode}
	// Provider text and request identifiers are untrusted and are not displayed.
	if seconds, err := strconv.ParseFloat(response.Header.Get("Retry-After"), 64); err == nil && seconds > 0 && seconds <= 30 {
		failure.RetryAfter = time.Duration(seconds * float64(time.Second))
	} else if date, err := http.ParseTime(response.Header.Get("Retry-After")); err == nil {
		failure.RetryAfter = max(0, time.Until(date))
	}
	return failure
}

type idleWatch struct {
	mu               sync.Mutex
	timer            *time.Timer
	timeout          time.Duration
	expired, stopped bool
	cancel           context.CancelFunc
}

func newIdleWatch(cancel context.CancelFunc, timeout time.Duration) *idleWatch {
	w := &idleWatch{timeout: timeout, cancel: cancel}
	w.timer = time.AfterFunc(timeout, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if !w.stopped {
			w.expired = true
			w.cancel()
		}
	})
	return w
}
func (w *idleWatch) pulse() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.stopped && !w.expired {
		w.timer.Reset(w.timeout)
	}
}
func (w *idleWatch) stop() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	w.timer.Stop()
	return w.expired
}

type activityReader struct {
	reader io.Reader
	watch  *idleWatch
}

func (r *activityReader) Read(p []byte) (int, error) {
	n, e := r.reader.Read(p)
	if n > 0 {
		r.watch.pulse()
	}
	return n, e
}
