// Package mcp implements the official Harness's existing MCP connection surface.
// It deliberately does not install servers or load Harness plugins.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dsgo/internal/core"
	"dsgo/internal/jsonschema"
	"dsgo/internal/state"
)

const ProtocolVersion = "2026-07-28"
const maxFrameBytes = 8 << 20
const requestTimeout = 60 * time.Second

var ErrClosed = errors.New("MCP connection closed")
var ErrIncompleteClose = errors.New("MCP transport closure could not be confirmed")

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("MCP RPC error %d: %s", e.Code, e.Message) }

type Notification struct {
	Method string
	Params json.RawMessage
}
type response struct {
	Result json.RawMessage
	Error  error
}
type wire struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type Client struct {
	ProtocolVersion string
	Instructions    string
	Capabilities    map[string]json.RawMessage
	ServerInfo      map[string]any

	cwd         string
	command     *exec.Cmd
	stdin       io.WriteCloser
	writer      chan struct{}
	processDone chan struct{}
	stderr      cappedLog

	mu            sync.Mutex
	pending       map[string]chan response
	terminalError error
	outputSchemas map[string]*jsonschema.Schema
	taskRequired  map[string]bool
	done          chan struct{}
	doneOnce      sync.Once
	closeOnce     sync.Once
	closeError    error
	seq           atomic.Int64
	notifications chan Notification
	http          *httpTransport
	lifetime      context.Context
	cancel        context.CancelFunc
}

// Open starts only an explicitly configured command, with direct argv and no
// shell interpolation. The caller owns approving that configured connection.
// Tool invocations still need the normal tool permission pipeline above Call.
func Open(ctx context.Context, cwd string, server state.MCPServer) (*Client, error) {
	if server.Disabled {
		return nil, errors.New("MCP server is disabled")
	}
	if (server.Command == "") == (server.URL == "") {
		return nil, errors.New("MCP requires exactly one of command and URL")
	}
	if server.CWD != "" {
		cwd = server.CWD
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	c := &Client{cwd: abs, writer: make(chan struct{}, 1), pending: map[string]chan response{}, done: make(chan struct{}), notifications: make(chan Notification, 64), processDone: make(chan struct{})}
	c.lifetime, c.cancel = context.WithCancel(context.Background())
	c.writer <- struct{}{}
	if server.URL != "" {
		c.http, err = newHTTP(server.URL, server.Headers)
		if err != nil {
			return nil, err
		}
	} else {
		cmd := exec.Command(server.Command, server.Args...)
		cmd.Dir = abs
		cmd.Env = scrubEnvironment(server.Env)
		cmd.Stderr = &c.stderr
		cmd.WaitDelay = 500 * time.Millisecond
		if err := setupProcess(cmd); err != nil {
			return nil, err
		}
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			stdin.Close()
			return nil, err
		}
		c.stdin = stdin
		c.command = cmd
		if err := cmd.Start(); err != nil {
			stdin.Close()
			stdout.Close()
			return nil, fmt.Errorf("start MCP server: %w", err)
		}
		go c.readLoop(stdout)
		go func() {
			err := cmd.Wait()
			// A server may leave descendants behind even after its own exit.
			_ = signalProcess(cmd, true)
			close(c.processDone)
			if err == nil {
				err = io.EOF
			}
			c.fail(fmt.Errorf("MCP server exited: %w", err))
		}()
	}
	var init struct {
		ProtocolVersion string                     `json:"protocolVersion"`
		Capabilities    map[string]json.RawMessage `json:"capabilities"`
		ServerInfo      map[string]any             `json:"serverInfo"`
		Instructions    string                     `json:"instructions"`
	}
	err = c.Call(ctx, "initialize", map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{"roots": map[string]any{"listChanged": false}}, "clientInfo": map[string]string{"name": "dsgo", "version": "0.1.0-dev"}}, &init)
	if err != nil {
		return nil, closeFailedClient(c, err)
	}
	if !supportedProtocol(init.ProtocolVersion) {
		return nil, closeFailedClient(c, fmt.Errorf("unsupported MCP protocol version %q", init.ProtocolVersion))
	}
	if len([]byte(init.Instructions)) > 32768 {
		return nil, closeFailedClient(c, errors.New("MCP server instructions exceed 32768 bytes"))
	}
	c.ProtocolVersion = init.ProtocolVersion
	if c.http != nil {
		c.http.mu.Lock()
		c.http.protocol = init.ProtocolVersion
		c.http.mu.Unlock()
	}
	c.Instructions = init.Instructions
	c.Capabilities = init.Capabilities
	c.ServerInfo = init.ServerInfo
	if err := c.notify(ctx, "notifications/initialized", nil); err != nil {
		return nil, closeFailedClient(c, err)
	}
	return c, nil
}

func closeFailedClient(c *Client, cause error) error {
	if err := c.Close(); err != nil {
		return errors.Join(cause, fmt.Errorf("%w: %v", ErrIncompleteClose, err))
	}
	return cause
}

func supportedProtocol(version string) bool {
	switch version {
	case ProtocolVersion, "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05":
		return true
	}
	return false
}

func scrubEnvironment(overrides map[string]string) []string {
	env := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		upper := strings.ToUpper(key)
		if strings.Contains(upper, "KEY") || strings.Contains(upper, "PASSWORD") || strings.Contains(upper, "SECRET") || strings.Contains(upper, "TOKEN") || strings.HasPrefix(upper, "DSH_") || strings.HasPrefix(upper, "DSGO_") {
			continue
		}
		env[key] = value
	}
	for key, value := range overrides {
		env[key] = value
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+env[key])
	}
	return result
}

func (c *Client) Notifications() <-chan Notification { return c.notifications }
func (c *Client) Stderr() string                     { return c.stderr.String() }

func (c *Client) fail(err error) {
	c.doneOnce.Do(func() {
		if c.cancel != nil {
			c.cancel()
		}
		c.mu.Lock()
		c.terminalError = err
		for id, ch := range c.pending {
			ch <- response{Error: err}
			delete(c.pending, id)
		}
		close(c.done)
		c.mu.Unlock()
	})
}

func (c *Client) Call(ctx context.Context, method string, params any, out any) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if c.lifetime != nil {
		stop := context.AfterFunc(c.lifetime, cancel)
		defer stop()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-c.done:
		return ErrClosed
	default:
	}
	id := strconv.FormatInt(c.seq.Add(1), 10)
	input, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if params == nil {
		input = nil
	}
	req := wire{JSONRPC: "2.0", ID: json.RawMessage(id), Method: method, Params: input}
	if c.http != nil {
		return c.httpCall(ctx, req, out)
	}
	ch := make(chan response, 1)
	c.mu.Lock()
	if c.terminalError != nil {
		err = c.terminalError
		c.mu.Unlock()
		return err
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	if err := c.write(ctx, req); err != nil {
		return err
	}
	select {
	case answer := <-ch:
		if answer.Error != nil {
			return answer.Error
		}
		if out != nil {
			if err := json.Unmarshal(answer.Result, out); err != nil {
				return fmt.Errorf("decode MCP %s result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		if method != "initialize" {
			cancelCtx, stop := context.WithTimeout(context.Background(), 250*time.Millisecond)
			_ = c.notify(cancelCtx, "notifications/cancelled", map[string]any{"requestId": json.RawMessage(id), "reason": "request cancelled"})
			stop()
		}
		return ctx.Err()
	}
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if params == nil {
		b = nil
	}
	w := wire{JSONRPC: "2.0", Method: method, Params: b}
	if c.http != nil {
		return c.httpNotify(ctx, w)
	}
	return c.write(ctx, w)
}

func (c *Client) write(ctx context.Context, message wire) error {
	b, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(b) > maxFrameBytes {
		return errors.New("MCP message exceeds 8 MiB")
	}
	b = append(b, '\n')
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return ErrClosed
	case <-c.writer:
	}
	defer func() { c.writer <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if file, ok := c.stdin.(*os.File); ok {
		deadline := time.Now().Add(requestTimeout)
		if d, ok := ctx.Deadline(); ok {
			deadline = d
		}
		_ = file.SetWriteDeadline(deadline)
		callbackDone := make(chan struct{})
		stop := context.AfterFunc(ctx, func() { _ = file.SetWriteDeadline(time.Now()); close(callbackDone) })
		defer func() {
			if !stop() {
				<-callbackDone
			}
			_ = file.SetWriteDeadline(time.Time{})
		}()
	}
	_, err = c.stdin.Write(b)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (c *Client) readLoop(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), maxFrameBytes)
	for scanner.Scan() {
		if err := c.handle(scanner.Bytes(), func(message wire) error {
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			return c.write(ctx, message)
		}); err != nil {
			c.fail(err)
			return
		}
	}
	if err := scanner.Err(); err != nil {
		c.fail(fmt.Errorf("read MCP frame: %w", err))
		return
	}
	c.fail(io.EOF)
}

func responseKey(id json.RawMessage) string { return string(bytes.TrimSpace(id)) }

func (c *Client) handle(data []byte, reply func(wire) error) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return errors.New("empty MCP JSON-RPC frame")
	}
	if trimmed[0] == '[' {
		var messages []json.RawMessage
		if err := json.Unmarshal(trimmed, &messages); err != nil {
			return fmt.Errorf("invalid MCP batch: %w", err)
		}
		if len(messages) == 0 {
			return errors.New("empty MCP batch")
		}
		for _, m := range messages {
			if err := c.handle(m, reply); err != nil {
				return err
			}
		}
		return nil
	}
	var message wire
	if err := json.Unmarshal(trimmed, &message); err != nil {
		return fmt.Errorf("invalid MCP JSON-RPC frame: %w", err)
	}
	if message.JSONRPC != "2.0" {
		return errors.New("MCP frame has unsupported JSON-RPC version")
	}
	if message.Method != "" {
		if len(message.ID) != 0 && string(message.ID) != "null" {
			answer := wire{JSONRPC: "2.0", ID: message.ID}
			switch message.Method {
			case "ping":
				answer.Result = json.RawMessage(`{}`)
			case "roots/list":
				uri := url.URL{Scheme: "file", Path: filepath.ToSlash(c.cwd)}
				answer.Result, _ = json.Marshal(map[string]any{"roots": []map[string]string{{"uri": uri.String(), "name": filepath.Base(c.cwd)}}})
			default:
				answer.Error = &RPCError{Code: -32601, Message: "client method not supported"}
			}
			return reply(answer)
		}
		select {
		case c.notifications <- Notification{Method: message.Method, Params: message.Params}:
		default:
		}
		return nil
	}
	if len(message.ID) == 0 {
		return errors.New("MCP response is missing request ID")
	}
	if (message.Error == nil) == (len(message.Result) == 0) {
		return errors.New("MCP response requires exactly one result or error")
	}
	key := responseKey(message.ID)
	c.mu.Lock()
	ch, ok := c.pending[key]
	if ok {
		delete(c.pending, key)
	}
	c.mu.Unlock()
	if ok {
		r := response{Result: message.Result}
		if message.Error != nil {
			r.Error = message.Error
		}
		ch <- r
	}
	return nil
}

func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.fail(ErrClosed)
		if c.http != nil {
			c.closeError = c.http.close()
			return
		}
		if c.stdin != nil {
			_ = c.stdin.Close()
		}
		if c.command == nil {
			return
		}
		select {
		case <-c.processDone:
			return
		case <-time.After(200 * time.Millisecond):
		}
		_ = signalProcess(c.command, false)
		select {
		case <-c.processDone:
			return
		case <-time.After(250 * time.Millisecond):
		}
		_ = signalProcess(c.command, true)
		select {
		case <-c.processDone:
		case <-time.After(time.Second):
			c.closeError = errors.New("MCP process did not exit after kill")
		}
	})
	return c.closeError
}

type cappedLog struct {
	mu sync.Mutex
	b  []byte
}

func (b *cappedLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	b.b = append(b.b, p...)
	if len(b.b) > 32768 {
		b.b = b.b[len(b.b)-32768:]
	}
	return n, nil
}
func (b *cappedLog) String() string { b.mu.Lock(); defer b.mu.Unlock(); return string(b.b) }

type Tool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Execution    struct {
		TaskSupport string `json:"taskSupport"`
	} `json:"execution,omitempty"`
}

type discoveredTools struct {
	specs   []core.ToolSpec
	outputs map[string]*jsonschema.Schema
	tasks   map[string]bool
}

func (c *Client) ListTools(ctx context.Context) ([]core.ToolSpec, error) {
	set, err := c.discoverTools(ctx)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.outputSchemas = set.outputs
	c.taskRequired = set.tasks
	c.mu.Unlock()
	return cloneToolSpecs(set.specs), nil
}

// cloneToolSpecs returns specs whose schemas callers may keep or modify.
func cloneToolSpecs(specs []core.ToolSpec) []core.ToolSpec {
	out := make([]core.ToolSpec, len(specs))
	for i, spec := range specs {
		spec.InputSchema = bytes.Clone(spec.InputSchema)
		out[i] = spec
	}
	return out
}

func (c *Client) discoverTools(ctx context.Context) (discoveredTools, error) {
	if _, ok := c.Capabilities["tools"]; !ok {
		return discoveredTools{specs: []core.ToolSpec{}, outputs: map[string]*jsonschema.Schema{}, tasks: map[string]bool{}}, nil
	}
	items, err := c.list(ctx, "tools/list", "tools")
	if err != nil {
		return discoveredTools{}, err
	}
	seen := map[string]bool{}
	outputs := map[string]*jsonschema.Schema{}
	tasks := map[string]bool{}
	result := make([]core.ToolSpec, 0, len(items))
	for _, item := range items {
		var tool Tool
		if err := json.Unmarshal(item, &tool); err != nil {
			return discoveredTools{}, err
		}
		if tool.Name == "" || seen[tool.Name] {
			return discoveredTools{}, fmt.Errorf("invalid or duplicate MCP tool name %q", tool.Name)
		}
		seen[tool.Name] = true
		var schema map[string]any
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil || schema == nil {
			return discoveredTools{}, fmt.Errorf("invalid input schema for MCP tool %q", tool.Name)
		}
		if schema["type"] != "object" {
			return discoveredTools{}, fmt.Errorf("MCP tool %q input schema must be an object", tool.Name)
		}
		if _, err := jsonschema.Compile(tool.InputSchema); err != nil {
			return discoveredTools{}, fmt.Errorf("invalid input schema for MCP tool %q: %w", tool.Name, err)
		}
		if len(tool.OutputSchema) > 0 {
			compiled, err := jsonschema.Compile(tool.OutputSchema)
			if err != nil {
				return discoveredTools{}, fmt.Errorf("invalid output schema for MCP tool %q: %w", tool.Name, err)
			}
			outputs[tool.Name] = compiled
		}
		tasks[tool.Name] = tool.Execution.TaskSupport == "required"
		result = append(result, core.ToolSpec{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema})
	}
	return discoveredTools{specs: result, outputs: outputs, tasks: tasks}, nil
}

func (c *Client) ListResources(ctx context.Context) ([]json.RawMessage, error) {
	if _, ok := c.Capabilities["resources"]; !ok {
		return []json.RawMessage{}, nil
	}
	return c.list(ctx, "resources/list", "resources")
}
func (c *Client) ListResourceTemplates(ctx context.Context) ([]json.RawMessage, error) {
	if _, ok := c.Capabilities["resources"]; !ok {
		return []json.RawMessage{}, nil
	}
	return c.list(ctx, "resources/templates/list", "resourceTemplates")
}

func (c *Client) list(ctx context.Context, method, key string) ([]json.RawMessage, error) {
	var result []json.RawMessage
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < 100; page++ {
		params := map[string]string{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var answer map[string]json.RawMessage
		if err := c.Call(ctx, method, params, &answer); err != nil {
			return nil, err
		}
		var items []json.RawMessage
		if err := json.Unmarshal(answer[key], &items); err != nil {
			return nil, fmt.Errorf("invalid MCP %s page: %w", method, err)
		}
		result = append(result, items...)
		if len(result) > 100000 {
			return nil, errors.New("MCP discovery exceeds 100000 entries")
		}
		cursor = ""
		if value, ok := answer["nextCursor"]; ok {
			if err := json.Unmarshal(value, &cursor); err != nil {
				return nil, err
			}
		}
		if cursor == "" {
			return result, nil
		}
		if seen[cursor] {
			return nil, errors.New("MCP discovery repeated pagination cursor")
		}
		seen[cursor] = true
	}
	return nil, errors.New("MCP discovery exceeds 100 pages")
}

type ToolResult struct {
	Content           []json.RawMessage `json:"content"`
	StructuredContent json.RawMessage   `json:"structuredContent,omitempty"`
	IsError           bool              `json:"isError,omitempty"`
}

func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
	c.mu.Lock()
	output := c.outputSchemas[name]
	required := c.taskRequired[name]
	c.mu.Unlock()
	return c.callToolChecked(ctx, name, args, output, required)
}

func (c *Client) callToolChecked(ctx context.Context, name string, args json.RawMessage, output *jsonschema.Schema, required bool) (ToolResult, error) {
	var result ToolResult
	if required {
		return result, errors.New("MCP tool requires task-based execution; the official bridge does not support this extension")
	}
	if !json.Valid(args) {
		return result, errors.New("MCP tool arguments must be valid JSON")
	}
	if err := c.Call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &result); err != nil {
		return result, err
	}
	if result.Content == nil {
		return result, errors.New("MCP tool result is missing content")
	}
	if result.IsError {
		return result, errors.New("MCP server returned isError tool result")
	}
	if err := validateToolResult(result); err != nil {
		return ToolResult{}, err
	}
	if output != nil {
		if len(result.StructuredContent) == 0 {
			return ToolResult{}, errors.New("MCP tool omitted structuredContent required by its output schema")
		}
		if err := output.ValidateJSON(result.StructuredContent); err != nil {
			return ToolResult{}, fmt.Errorf("MCP tool output failed advertised schema: %w", err)
		}
	}
	return result, nil
}

func validateToolResult(result ToolResult) error {
	for _, raw := range result.Content {
		var block map[string]json.RawMessage
		if err := json.Unmarshal(raw, &block); err != nil || block == nil {
			return errors.New("invalid MCP content block")
		}
		var kind string
		if json.Unmarshal(block["type"], &kind) != nil || kind == "" {
			return errors.New("MCP content block is missing its type")
		}
		fields := []string{}
		switch kind {
		case "text":
			fields = []string{"text"}
		case "image", "audio":
			fields = []string{"data", "mimeType"}
		case "resource_link":
			fields = []string{"name", "uri"}
		}
		for _, field := range fields {
			var value string
			if json.Unmarshal(block[field], &value) != nil || string(block[field]) == "null" {
				return fmt.Errorf("invalid MCP %s content: %s must be a string", kind, field)
			}
		}
	}
	if len(result.StructuredContent) > 0 {
		var object map[string]json.RawMessage
		if json.Unmarshal(result.StructuredContent, &object) != nil || object == nil {
			return errors.New("MCP structuredContent must be an object")
		}
	}
	return nil
}

var validServerName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

func ValidServerName(name string) bool { return validServerName.MatchString(name) }
