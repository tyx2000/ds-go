package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"
)

type sseEvent struct {
	data     []byte
	kind     string
	id       string
	hasID    bool
	retry    time.Duration
	hasRetry bool
}

// parseSSEEvents retains the replay cursor, including empty priming events.
// Like the fixed SDK's EventSourceParserStream, HTTP does not inherit stdio's
// read-buffer ceiling. The caller's request/lifetime cancellation owns the read.
func parseSSEEvents(reader io.Reader, emit func(sseEvent) error) error {
	event := sseEvent{}
	hasData := false
	flush := func() error {
		var err error
		if hasData {
			event.data = bytes.TrimSuffix(event.data, []byte{'\n'})
			err = emit(event)
		}
		event = sseEvent{}
		hasData = false
		return err
	}
	err := readSSELines(reader, func(line []byte) error {
		if len(line) == 0 {
			if err := flush(); err != nil {
				return err
			}
			return nil
		}
		if line[0] == ':' {
			return nil
		}
		key, value, ok := bytes.Cut(line, []byte{':'})
		if !ok {
			key, value = line, nil
		}
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		switch string(key) {
		case "data":
			hasData = true
			event.data = append(event.data, value...)
			event.data = append(event.data, '\n')
		case "event":
			event.kind = string(value)
		case "id":
			event.id = string(value)
			event.hasID = len(value) > 0 && !bytes.ContainsRune(value, '\x00')
		case "retry":
			if len(value) == 0 || bytes.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
				return nil
			}
			if n, err := strconv.ParseInt(string(value), 10, 32); err == nil {
				// onRetry is independent of event dispatch, even without a data
				// field or a following blank line.
				return emit(sseEvent{retry: time.Duration(n) * time.Millisecond, hasRetry: true})
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	// EventSourceParserStream has no flush hook: incomplete EOF blocks vanish.
	return nil
}

func waitSSE(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func sseDelay(attempt int, supplied time.Duration) time.Duration {
	if supplied >= 0 {
		return supplied
	}
	if attempt == 0 {
		return time.Second
	}
	return 1500 * time.Millisecond
}

func sseResponse(res *http.Response) error {
	kind, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if kind != "text/event-stream" {
		return fmt.Errorf("unsupported MCP GET content type %q", kind)
	}
	return nil
}

// readHTTPEvents owns the optional server-initiated GET stream. Recovery uses
// only GET+Last-Event-ID; an interrupted mutation is never POSTed a second time.
func (c *Client) readHTTPEvents() {
	ctx := c.lifetime
	lastID := ""
	delay := time.Duration(-1)
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		res, err := c.http.requestWithResume(ctx, http.MethodGet, nil, c.ProtocolVersion, lastID)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			var status *HTTPStatusError
			if errors.As(err, &status) && status.StatusCode == http.StatusMethodNotAllowed {
				return
			}
			if errors.Is(err, ErrSessionExpired) {
				return
			}
		} else {
			attempt = 0 // An opened stream closes the failed-open retry sequence.
			err = sseResponse(res)
			if err == nil {
				err = parseSSEEvents(res.Body, func(event sseEvent) error {
					if event.hasRetry {
						delay = event.retry
					}
					if event.hasID {
						lastID = event.id
					}
					if len(event.data) > 0 && (event.kind == "" || event.kind == "message") {
						if err := c.handle(event.data, func(message wire) error { return c.httpNotify(ctx, message) }); err != nil {
							return err
						}
					}
					return nil
				})
			}
			res.Body.Close()
		}
		if ctx.Err() != nil {
			return
		}
		if attempt >= 2 {
			if err == nil {
				err = errors.New("MCP HTTP notification stream ended")
			}
			return
		}
		if err := waitSSE(ctx, sseDelay(attempt, delay)); err != nil {
			return
		}
		attempt++
	}
}

func (c *Client) readHTTPReplyStream(ctx context.Context, initial *http.Response, ch chan response, reply func(wire) error) error {
	res := initial
	lastID := ""
	delay := time.Duration(-1)
	complete := errors.New("matching MCP response received")
	for {
		err := parseSSEEvents(res.Body, func(event sseEvent) error {
			if event.hasRetry {
				delay = event.retry
			}
			if event.hasID {
				lastID = event.id
			}
			if len(event.data) > 0 && (event.kind == "" || event.kind == "message") {
				if err := c.handle(event.data, reply); err != nil {
					return err
				}
			}
			if len(ch) > 0 {
				return complete
			}
			return nil
		})
		res.Body.Close()
		if errors.Is(err, complete) {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if lastID == "" {
			if err != nil {
				return err
			}
			return errors.New("MCP HTTP stream ended without matching response")
		}
		for attempt := 0; ; attempt++ {
			if err := waitSSE(ctx, sseDelay(attempt, delay)); err != nil {
				return err
			}
			var getErr error
			res, getErr = c.http.requestWithResume(ctx, http.MethodGet, nil, c.ProtocolVersion, lastID)
			if getErr == nil {
				break
			}
			if errors.Is(getErr, ErrSessionExpired) {
				return getErr
			}
			var status *HTTPStatusError
			if errors.As(getErr, &status) && status.StatusCode == http.StatusMethodNotAllowed {
				return getErr
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if attempt >= 1 {
				return getErr
			}
		}
		if err := sseResponse(res); err != nil {
			res.Body.Close()
			return err
		}
	}
}
