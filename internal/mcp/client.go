// Package mcp implements the official Harness's existing MCP connection surface.
// It deliberately does not install servers or load Harness plugins.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
const LegacyProtocolVersion = "2025-11-25"

// The fixed SDK bounds only unread stdio input, not outgoing messages or HTTP.
const stdioReadBufferBytes = 10 << 20
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

	mu             sync.Mutex
	pending        map[string]chan response
	terminalError  error
	outputSchemas  map[string]*jsonschema.Schema
	outputErrors   map[string]error
	paramHeaders   map[string][]paramHeaderDeclaration
	taskRequired   map[string]bool
	done           chan struct{}
	doneOnce       sync.Once
	closeOnce      sync.Once
	closeError     error
	seq            atomic.Int64
	notifications  chan Notification
	warnings       chan string
	http           *httpTransport
	lifetime       context.Context
	cancel         context.CancelFunc
	callTimeout    time.Duration
	toolsChanged   chan struct{}
	httpWG         sync.WaitGroup
	probing        bool
	probeTransport bool
	probeID        string
	listen         *subscription
	cache          responseCache
}

type OpenOptions struct {
	CallTimeout          time.Duration
	ProbeTimeout         time.Duration
	MaxInstructionBytes  int
	skipInstructionLimit bool // supervisor checks the trimmed, attributed value
}

// Open starts only an explicitly configured command, with direct argv and no
// shell interpolation. The caller owns approving that configured connection.
// Tool invocations still need the normal tool permission pipeline above Call.
func Open(ctx context.Context, cwd string, server state.MCPServer) (*Client, error) {
	return OpenWithOptions(ctx, cwd, server, OpenOptions{})
}

func OpenWithOptions(ctx context.Context, cwd string, server state.MCPServer, options OpenOptions) (*Client, error) {
	if options.CallTimeout == 0 {
		options.CallTimeout = requestTimeout
	}
	if options.MaxInstructionBytes == 0 {
		options.MaxInstructionBytes = 32768
	}
	if options.CallTimeout < 0 || options.MaxInstructionBytes < 1 {
		return nil, errors.New("invalid MCP connection limits")
	}
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
	return openNegotiated(ctx, abs, server, options)
}

func startTransport(abs string, server state.MCPServer, options OpenOptions, probing bool) (*Client, error) {
	var err error
	c := &Client{cwd: abs, writer: make(chan struct{}, 1), pending: map[string]chan response{}, done: make(chan struct{}), notifications: make(chan Notification, 64), warnings: make(chan string, 64), processDone: make(chan struct{}), callTimeout: options.CallTimeout, toolsChanged: make(chan struct{}, 1), probing: probing, probeTransport: probing && server.Command != ""}
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
	case ProtocolVersion, "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05", "2024-10-07":
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
func (c *Client) Done() <-chan struct{}              { return c.done }
func (c *Client) Err() error                         { c.mu.Lock(); defer c.mu.Unlock(); return c.terminalError }

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
	return c.callWithInputDriver(ctx, method, params, out)
}

func (c *Client) callWithID(ctx context.Context, method string, params any, out any, id json.RawMessage, raw bool) error {
	timeout := c.callTimeout
	if raw {
		timeout = requestTimeout
	}
	if timeout == 0 {
		timeout = requestTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
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
	input, err := c.params(params, method == "server/discover" && raw)
	if err != nil {
		return err
	}
	req := wire{JSONRPC: "2.0", ID: id, Method: method, Params: input}
	if c.http != nil {
		var result json.RawMessage
		if err := c.httpCall(ctx, req, &result); err != nil {
			return err
		}
		return c.decodeResult(method, result, out, raw)
	}
	key := responseKey(id)
	ch := make(chan response, 1)
	c.mu.Lock()
	if c.terminalError != nil {
		err = c.terminalError
		c.mu.Unlock()
		return err
	}
	c.pending[key] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, key); c.mu.Unlock() }()
	if err := c.write(ctx, req); err != nil {
		return err
	}
	select {
	case answer := <-ch:
		if answer.Error != nil {
			return answer.Error
		}
		return c.decodeResult(method, answer.Result, out, raw)
	case <-ctx.Done():
		if method != "initialize" && !raw {
			cancelCtx, stop := context.WithTimeout(context.Background(), 250*time.Millisecond)
			_ = c.notify(cancelCtx, "notifications/cancelled", map[string]any{"requestId": id, "reason": "request cancelled"})
			stop()
		}
		return ctx.Err()
	}
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	b, err := c.params(params, false)
	if err != nil {
		return err
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
	chunk := make([]byte, 64<<10)
	var buffered []byte
	for {
		n, readErr := r.Read(chunk)
		if len(buffered)+n > stdioReadBufferBytes {
			c.fail(fmt.Errorf("MCP stdio read buffer exceeds %d bytes", stdioReadBufferBytes))
			go c.Close()
			return
		}
		buffered = append(buffered, chunk[:n]...)
		for {
			end := bytes.IndexByte(buffered, '\n')
			if end < 0 {
				break
			}
			line := bytes.TrimSuffix(buffered[:end], []byte{'\r'})
			// ReadBuffer skips non-JSON stdout lines, including debug banners.
			if json.Valid(line) {
				if err := c.handle(line, func(message wire) error {
					ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
					defer cancel()
					return c.write(ctx, message)
				}); err != nil {
					// Schema-invalid frames report onerror without closing the SDK
					// transport; later valid frames must still be delivered.
					select {
					case c.warnings <- err.Error():
					default:
					}
				}
			}
			buffered = buffered[end+1:]
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				c.fail(fmt.Errorf("read MCP frame: %w", readErr))
			} else {
				c.fail(io.EOF)
			}
			return
		}
	}
}

func responseKey(id json.RawMessage) string { return string(bytes.TrimSpace(id)) }

func (c *Client) handle(data []byte, reply func(wire) error) error {
	c.mu.Lock()
	probing, probeID := c.probing, c.probeID
	c.mu.Unlock()
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		if probing {
			return nil
		}
		return errors.New("empty MCP JSON-RPC frame")
	}
	if trimmed[0] == '[' {
		if probing {
			return nil
		}
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
		if probing {
			return nil
		}
		return fmt.Errorf("invalid MCP JSON-RPC frame: %w", err)
	}
	if probing && (message.Method != "" || responseKey(message.ID) != strconv.Quote(probeID)) {
		return nil
	}
	if message.JSONRPC != "2.0" {
		if probing {
			return nil
		}
		return errors.New("MCP frame has unsupported JSON-RPC version")
	}
	if message.Method != "" {
		if len(message.ID) != 0 && string(message.ID) != "null" {
			if c.modern() {
				return nil
			}
			answer := wire{JSONRPC: "2.0", ID: message.ID}
			switch message.Method {
			case "ping":
				answer.Result = json.RawMessage(`{}`)
			default:
				answer.Error = &RPCError{Code: -32601, Message: "client method not supported"}
			}
			return reply(answer)
		}
		c.invalidateCache(message)
		if c.routeSubscriptionNotification(message) {
			return nil
		}
		select {
		case c.notifications <- Notification{Method: message.Method, Params: message.Params}:
		default:
		}
		if message.Method == "notifications/tools/list_changed" && c.toolsChanged != nil && !c.modern() && c.toolsListChanged() {
			select {
			case c.toolsChanged <- struct{}{}:
			default:
			}
		}
		return nil
	}
	if len(message.ID) == 0 {
		return errors.New("MCP response is missing request ID")
	}
	if (message.Error == nil) == (len(message.Result) == 0) {
		return errors.New("MCP response requires exactly one result or error")
	}
	if c.routeSubscriptionResponse(message) {
		return nil
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
		c.closeToolsSubscription()
		c.fail(ErrClosed)
		c.cache.reset()
		if c.http != nil {
			c.httpWG.Wait()
			c.closeError = c.http.close()
			return
		}
		if c.stdin != nil {
			_ = c.stdin.Close()
		}
		if c.command == nil {
			return
		}
		if !c.probeTransport {
			select {
			case <-c.processDone:
				return
			case <-time.After(2 * time.Second):
			}
		}
		_ = signalProcess(c.command, false)
		grace := 2 * time.Second
		if c.probeTransport {
			grace = time.Second
		}
		select {
		case <-c.processDone:
			return
		case <-time.After(grace):
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
	specs        []core.ToolSpec
	outputs      map[string]*jsonschema.Schema
	outputErrors map[string]error
	paramHeaders map[string][]paramHeaderDeclaration
	tasks        map[string]bool
}

func (c *Client) ListTools(ctx context.Context) ([]core.ToolSpec, error) {
	set, err := c.discoverTools(ctx, false)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.outputSchemas = set.outputs
	c.outputErrors = set.outputErrors
	c.paramHeaders = set.paramHeaders
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

func (c *Client) discoverTools(ctx context.Context, refreshOption ...bool) (discoveredTools, error) {
	refresh := len(refreshOption) == 0 || refreshOption[0]
	if _, ok := c.Capabilities["tools"]; !ok {
		return discoveredTools{specs: []core.ToolSpec{}, outputs: map[string]*jsonschema.Schema{}, tasks: map[string]bool{}}, nil
	}
	items, err := c.cachedList(ctx, "tools/list", "tools", refresh)
	if err != nil {
		return discoveredTools{}, err
	}
	seen := map[string]bool{}
	outputs := map[string]*jsonschema.Schema{}
	outputErrors := map[string]error{}
	paramHeaders := map[string][]paramHeaderDeclaration{}
	tasks := map[string]bool{}
	result := make([]core.ToolSpec, 0, len(items))
	for _, item := range items {
		var tool Tool
		if err := json.Unmarshal(item, &tool); err != nil {
			return discoveredTools{}, err
		}
		if seen[tool.Name] {
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
		// The official raw MCP ToolDefinition retains parameters without
		// compiling or validating them locally. Wire shape validation above
		// is separate from the server's argument/schema semantics.
		if len(tool.OutputSchema) > 0 {
			compiled, err := jsonschema.Compile(tool.OutputSchema)
			if err != nil {
				outputErrors[tool.Name] = fmt.Errorf("invalid output schema for MCP tool %q: %w", tool.Name, err)
			} else {
				outputs[tool.Name] = compiled
			}
		}
		tasks[tool.Name] = tool.Execution.TaskSupport == "required"
		if c.http != nil && c.modern() {
			paramHeaders[tool.Name], _ = scanParamHeaders(tool.InputSchema)
		}
		result = append(result, core.ToolSpec{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema})
	}
	return discoveredTools{specs: result, outputs: outputs, outputErrors: outputErrors, paramHeaders: paramHeaders, tasks: tasks}, nil
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
	return c.cachedList(ctx, method, key, false)
}

type ToolResult struct {
	Content           []json.RawMessage `json:"content"`
	StructuredContent json.RawMessage   `json:"structuredContent,omitempty"`
	IsError           bool              `json:"isError,omitempty"`
}

func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
	c.mu.Lock()
	output := c.outputSchemas[name]
	compileError := c.outputErrors[name]
	headers := c.paramHeaders[name]
	required := c.taskRequired[name]
	c.mu.Unlock()
	return c.callToolChecked(withParamHeaders(ctx, headers, args), name, args, output, required, compileError)
}

func (c *Client) callToolChecked(ctx context.Context, name string, args json.RawMessage, output *jsonschema.Schema, required bool, compileError error) (ToolResult, error) {
	var result ToolResult
	if compileError != nil {
		return result, compileError
	}
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
	if err := validateToolResultForEra(result, c.modern()); err != nil {
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
	return validateToolResultForEra(result, false)
}

func validateToolResultForEra(result ToolResult, modern bool) error {
	return validateResultWire("tools/call", mustJSON(result), modern)
}

var validServerName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

func ValidServerName(name string) bool { return validServerName.MatchString(name) }
