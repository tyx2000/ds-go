package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"
)

type subscription struct {
	id                            string
	ctx                           context.Context
	cancel                        context.CancelFunc
	ack                           chan error
	done                          chan struct{}
	acknowledged, closed, honored bool // protected by Client.mu
}

func (c *Client) finishSubscription(sub *subscription, cause error) {
	c.mu.Lock()
	if c.listen != sub || sub.closed {
		c.mu.Unlock()
		return
	}
	sub.closed = true
	if !sub.acknowledged {
		if cause == nil {
			cause = errors.New("MCP subscription closed before acknowledgement")
		}
		sub.ack <- cause
	}
	close(sub.done)
	c.mu.Unlock()
	sub.cancel()
}

func (c *Client) openToolsSubscription(connectCtx context.Context) error {
	if !c.toolsListChanged() {
		return nil
	}
	ctx, cancel := context.WithCancel(c.lifetime)
	sub := &subscription{id: "listen:0", ctx: ctx, cancel: cancel, ack: make(chan error, 1), done: make(chan struct{})}
	c.mu.Lock()
	c.listen = sub
	c.mu.Unlock()
	params, err := c.params(map[string]any{"notifications": map[string]bool{"toolsListChanged": true}}, false)
	if err != nil {
		c.finishSubscription(sub, err)
		return err
	}
	request := wire{JSONRPC: "2.0", ID: json.RawMessage(strconv.Quote(sub.id)), Method: "subscriptions/listen", Params: params}
	if c.http != nil {
		c.httpWG.Add(1)
		go func() { defer c.httpWG.Done(); c.serveHTTPSubscription(sub, request) }()
	} else {
		go func() {
			if err := c.write(sub.ctx, request); err != nil {
				c.finishSubscription(sub, err)
			}
		}()
	}
	timer := time.NewTimer(requestTimeout)
	defer timer.Stop()
	select {
	case err := <-sub.ack:
		return err
	case <-connectCtx.Done():
		c.closeToolsSubscription()
		return connectCtx.Err()
	case <-c.done:
		c.finishSubscription(sub, c.Err())
		return c.Err()
	case <-timer.C:
		c.closeToolsSubscription()
		return errors.New("MCP subscriptions/listen acknowledgement timed out")
	}
}

func (c *Client) closeToolsSubscription() {
	c.mu.Lock()
	sub := c.listen
	active := sub != nil && !sub.closed
	c.mu.Unlock()
	if !active {
		return
	}
	c.finishSubscription(sub, errors.New("MCP subscription closed locally"))
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_ = c.notify(ctx, "notifications/cancelled", map[string]any{"requestId": sub.id})
}

func (c *Client) routeSubscriptionNotification(message wire) bool {
	if !c.modern() {
		return false
	}
	var params struct {
		Meta          map[string]json.RawMessage `json:"_meta"`
		Notifications map[string]json.RawMessage `json:"notifications"`
		RequestID     json.RawMessage            `json:"requestId"`
	}
	_ = json.Unmarshal(message.Params, &params)
	var id string
	_ = json.Unmarshal(params.Meta[subscriptionMetaKey], &id)
	c.mu.Lock()
	sub := c.listen
	if sub == nil || sub.closed {
		c.mu.Unlock()
		return message.Method == "notifications/tools/list_changed"
	}
	if message.Method == "notifications/cancelled" && string(params.RequestID) == strconv.Quote(sub.id) {
		c.mu.Unlock()
		c.finishSubscription(sub, errors.New("MCP server cancelled subscription"))
		return true
	}
	if id != sub.id {
		c.mu.Unlock()
		return message.Method == "notifications/tools/list_changed"
	}
	if message.Method == "notifications/subscriptions/acknowledged" {
		if !sub.acknowledged {
			honored := validHonoredTools(params.Notifications)
			sub.acknowledged, sub.honored = true, honored
			sub.ack <- nil
		}
		c.mu.Unlock()
		return true
	}
	allow := sub.acknowledged && sub.honored
	c.mu.Unlock()
	if message.Method == "notifications/tools/list_changed" {
		if allow {
			select {
			case c.toolsChanged <- struct{}{}:
			default:
			}
		}
		return true
	}
	return false
}

func (c *Client) toolsListChanged() bool {
	c.mu.Lock()
	raw := c.Capabilities["tools"]
	c.mu.Unlock()
	var tools struct {
		ListChanged bool `json:"listChanged"`
	}
	return json.Unmarshal(raw, &tools) == nil && tools.ListChanged
}

func validHonoredTools(filter map[string]json.RawMessage) bool {
	honored := false
	for _, name := range []string{"toolsListChanged", "promptsListChanged", "resourcesListChanged"} {
		if raw, ok := filter[name]; ok {
			var value bool
			if string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
				return false
			}
			if name == "toolsListChanged" {
				honored = value
			}
		}
	}
	if raw, ok := filter["resourceSubscriptions"]; ok {
		var values []string
		if string(raw) == "null" || json.Unmarshal(raw, &values) != nil {
			return false
		}
	}
	return honored
}

func (c *Client) routeSubscriptionResponse(message wire) bool {
	c.mu.Lock()
	sub := c.listen
	match := sub != nil && !sub.closed && string(message.ID) == strconv.Quote(sub.id)
	c.mu.Unlock()
	if !match {
		return false
	}
	var cause error = errors.New("MCP subscription ended gracefully")
	if message.Error != nil {
		cause = message.Error
	}
	c.finishSubscription(sub, cause)
	return true
}

func (c *Client) serveHTTPSubscription(sub *subscription, request wire) {
	body, err := json.Marshal(request)
	if err != nil {
		c.finishSubscription(sub, err)
		return
	}
	res, err := c.http.request(sub.ctx, http.MethodPost, body, c.ProtocolVersion)
	if err != nil {
		if rpc := matchingHTTPError(err, request); rpc != nil {
			err = rpc
		}
		c.finishSubscription(sub, err)
		return
	}
	typeName, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if typeName == "application/json" {
		data, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err == nil {
			err = c.handle(data, func(message wire) error { return c.httpNotify(sub.ctx, message) })
		}
		c.finishSubscription(sub, err)
		return
	}
	if typeName != "text/event-stream" {
		res.Body.Close()
		c.finishSubscription(sub, fmt.Errorf("unsupported MCP subscription content type %q", typeName))
		return
	}
	lastID := ""
	retry := time.Duration(-1)
	for {
		err = parseSSEEvents(res.Body, func(event sseEvent) error {
			if event.hasRetry {
				retry = event.retry
			}
			if event.hasID {
				lastID = event.id
			}
			if len(event.data) > 0 && (event.kind == "" || event.kind == "message") {
				if err := c.handle(event.data, func(message wire) error { return c.httpNotify(sub.ctx, message) }); err != nil {
					return err
				}
			}
			select {
			case <-sub.done:
				return io.EOF
			default:
				return nil
			}
		})
		res.Body.Close()
		if sub.ctx.Err() != nil {
			return
		}
		if lastID == "" {
			c.finishSubscription(sub, errors.New("MCP subscription stream ended"))
			return
		}
		for attempt := 0; ; attempt++ {
			if err := waitSSE(sub.ctx, sseDelay(attempt, retry)); err != nil {
				return
			}
			res, err = c.http.requestWithResume(sub.ctx, http.MethodGet, nil, c.ProtocolVersion, lastID)
			if err == nil {
				break
			}
			var status *HTTPStatusError
			if attempt >= 1 || errors.Is(err, ErrSessionExpired) || errors.As(err, &status) && status.StatusCode == 405 {
				c.finishSubscription(sub, err)
				return
			}
		}
		if err := sseResponse(res); err != nil {
			res.Body.Close()
			c.finishSubscription(sub, err)
			return
		}
	}
}
