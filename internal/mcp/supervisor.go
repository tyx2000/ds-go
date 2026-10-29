package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"dsgo/internal/core"
	"dsgo/internal/state"
)

var ErrDisconnected = errors.New("MCP server is disconnected")

// ReconnectPolicy mirrors the official connection supervisor. Zero values use
// its defaults; Enabled is a pointer so an explicit false remains meaningful.
type ReconnectPolicy struct {
	Enabled                *bool
	InitialDelay, MaxDelay time.Duration
	MaxAttempts            int
}

type SupervisorOptions struct {
	Name, CWD                       string
	Server                          state.MCPServer
	ConnectTimeout, ToolCallTimeout time.Duration
	MaxInstructionBytes             int
	FailOnStartupError              bool
	Reconnect                       *ReconnectPolicy
	// Publish is serialized and must not call back into this supervisor. It
	// installs a complete server-owned tool group, or rejects it without leaving
	// a partial group. Instructions are already attributed to the server.
	Publish func(SupervisorSnapshot) error
	Log     func(string)
}

type SupervisorSnapshot struct {
	Name         string
	Generation   uint64
	Status       string
	Tools        []core.ToolSpec
	Instructions string
	Error        string
	Attempts     int
}

type Supervisor struct {
	options     SupervisorOptions
	policy      ReconnectPolicy
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	closeOnce   sync.Once
	mu          sync.Mutex
	snapshot    SupervisorSnapshot
	client      *Client
	set         discoveredTools
	connectedAt time.Time
	closeError  error
}

func resolveReconnect(input *ReconnectPolicy) (ReconnectPolicy, error) {
	enabled := true
	p := ReconnectPolicy{Enabled: &enabled, InitialDelay: 500 * time.Millisecond, MaxDelay: 30 * time.Second, MaxAttempts: 10}
	if input != nil {
		if input.Enabled != nil {
			enabled = *input.Enabled
		}
		if input.InitialDelay != 0 {
			p.InitialDelay = input.InitialDelay
		}
		if input.MaxDelay != 0 {
			p.MaxDelay = input.MaxDelay
		}
		if input.MaxAttempts != 0 {
			p.MaxAttempts = input.MaxAttempts
		}
	}
	const maxTimer = time.Duration(2147483647) * time.Millisecond
	if p.InitialDelay <= 0 || p.InitialDelay > maxTimer || p.MaxDelay <= 0 || p.MaxDelay > maxTimer || p.InitialDelay > p.MaxDelay || p.MaxAttempts < 1 {
		return p, errors.New("invalid MCP reconnect policy")
	}
	return p, nil
}

// OpenSupervisor waits for the initial discovery attempt, never for recovery.
// An ordinary startup failure is logged and retried without aborting Harness.
func OpenSupervisor(ctx context.Context, options SupervisorOptions) (*Supervisor, error) {
	if !ValidServerName(options.Name) {
		return nil, errors.New("invalid MCP server name")
	}
	policy, err := resolveReconnect(options.Reconnect)
	if err != nil {
		return nil, err
	}
	if options.ToolCallTimeout == 0 {
		options.ToolCallTimeout = requestTimeout
	}
	if options.MaxInstructionBytes == 0 {
		options.MaxInstructionBytes = 32768
	}
	if options.ConnectTimeout < 0 || options.ToolCallTimeout < 0 || options.MaxInstructionBytes < 1 {
		return nil, errors.New("invalid MCP supervisor limits")
	}
	lifetime, cancel := context.WithCancel(ctx)
	s := &Supervisor{options: options, policy: policy, ctx: lifetime, cancel: cancel, done: make(chan struct{}), snapshot: SupervisorSnapshot{Name: options.Name, Status: "connecting", Tools: []core.ToolSpec{}}}
	err = s.connect()
	if err != nil {
		s.log("initial connection failed: " + err.Error())
		s.changeStatus("disconnected", err)
		if options.FailOnStartupError || ctx.Err() != nil {
			cancel()
			s.clear("closed", err)
			close(s.done)
			return nil, err
		}
	}
	go s.run(err)
	return s, nil
}

func (s *Supervisor) log(message string) {
	if s.options.Log != nil {
		s.options.Log("mcp-client(" + s.options.Name + "): " + message)
	}
}

func cloneSnapshot(snapshot SupervisorSnapshot) SupervisorSnapshot {
	snapshot.Tools = cloneToolSpecs(snapshot.Tools)
	return snapshot
}

func (s *Supervisor) Snapshot() SupervisorSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneSnapshot(s.snapshot)
}

func (s *Supervisor) publishLocked(next SupervisorSnapshot) error {
	if s.options.Publish != nil {
		if err := s.options.Publish(cloneSnapshot(next)); err != nil {
			return err
		}
	}
	s.snapshot = next
	return nil
}

func (s *Supervisor) changeStatus(status string, cause error) {
	s.mu.Lock()
	next := s.snapshot
	next.Status = status
	next.Error = ""
	if cause != nil {
		next.Error = cause.Error()
	}
	publicationErr := s.publishLocked(next)
	if publicationErr != nil {
		s.snapshot = next
	}
	s.mu.Unlock()
	if publicationErr != nil {
		s.log("status publication failed: " + publicationErr.Error())
	}
}

func (s *Supervisor) clear(status string, cause error) {
	s.mu.Lock()
	next := s.snapshot
	next.Status, next.Tools, next.Instructions = status, []core.ToolSpec{}, ""
	next.Error = ""
	if cause != nil {
		next.Error = cause.Error()
	}
	if err := s.publishLocked(next); err != nil {
		s.snapshot = next
		s.closeError = errors.Join(s.closeError, err)
	}
	s.set = discoveredTools{}
	s.mu.Unlock()
}

func (s *Supervisor) connect() error {
	ctx := s.ctx
	if s.options.ConnectTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.options.ConnectTimeout)
		defer cancel()
	}
	client, err := OpenWithOptions(ctx, s.options.CWD, s.options.Server, OpenOptions{CallTimeout: s.options.ToolCallTimeout, MaxInstructionBytes: s.options.MaxInstructionBytes, skipInstructionLimit: true})
	if err != nil {
		return err
	}
	closeOnFailure := func(cause error) error { return closeFailedClient(client, cause) }
	text := strings.TrimRightFunc(client.Instructions, func(r rune) bool {
		switch r {
		case '\t', '\v', '\f', ' ', '\u00a0', '\ufeff', '\n', '\r', '\u2028', '\u2029', '\u1680', '\u202f', '\u205f', '\u3000':
			return true
		}
		return r >= '\u2000' && r <= '\u200a'
	})
	instructions := ""
	if text != "" {
		instructions = "### MCP server: " + s.options.Name + "\n\n" + text
	}
	if len([]byte(instructions)) > s.options.MaxInstructionBytes {
		return closeOnFailure(fmt.Errorf("server instructions exceed maxInstructionBytes (%d)", s.options.MaxInstructionBytes))
	}
	set, err := client.discoverTools(ctx)
	if err != nil {
		return closeOnFailure(err)
	}
	select {
	case <-client.Done():
		return closeOnFailure(client.Err())
	default:
	}
	if err := s.ctx.Err(); err != nil {
		return closeOnFailure(err)
	}
	s.mu.Lock()
	next := s.snapshot
	next.Generation++
	next.Status, next.Error, next.Tools, next.Instructions = "connected", "", set.specs, instructions
	err = s.publishLocked(next)
	if err == nil {
		s.client, s.set, s.connectedAt = client, set, time.Now()
	}
	s.mu.Unlock()
	if err != nil {
		s.clearToolsAfterConflict(err)
		return closeOnFailure(err)
	}
	s.log("connected and synchronized tools")
	return nil
}

// A registry conflict is contained as one empty group, matching the official
// dispose-previous/register-next transaction. Discovery errors never enter here.
func (s *Supervisor) clearToolsAfterConflict(cause error) {
	s.mu.Lock()
	next := s.snapshot
	next.Tools, next.Error = []core.ToolSpec{}, cause.Error()
	publicationErr := s.publishLocked(next)
	if publicationErr != nil {
		s.snapshot = next
	}
	s.set = discoveredTools{}
	s.mu.Unlock()
	if publicationErr != nil {
		s.log("tool rollback failed: " + publicationErr.Error())
	}
}

func (s *Supervisor) refresh(client *Client) {
	ctx, cancel := context.WithTimeout(s.ctx, s.options.ToolCallTimeout)
	defer cancel()
	set, err := client.discoverTools(ctx)
	if err != nil {
		s.log("tool re-sync failed; previous tools retained: " + err.Error())
		return
	}
	s.mu.Lock()
	if s.client != client || s.ctx.Err() != nil {
		s.mu.Unlock()
		return
	}
	next := s.snapshot
	next.Tools, next.Error = set.specs, ""
	err = s.publishLocked(next)
	if err == nil {
		s.set = set
	}
	s.mu.Unlock()
	if err != nil {
		s.log("tool registration failed: " + err.Error())
		s.clearToolsAfterConflict(err)
	}
}

func (s *Supervisor) run(initialError error) {
	defer close(s.done)
	err := initialError
	for {
		if err == nil {
			s.mu.Lock()
			client := s.client
			s.mu.Unlock()
			if client == nil {
				err = ErrDisconnected
			} else {
			watch:
				for {
					select {
					case <-s.ctx.Done():
						break watch
					case <-client.Done():
						err = client.Err()
						break watch
					case <-client.toolsChanged:
						s.refresh(client)
					case warning := <-client.warnings:
						s.log(warning)
					}
				}
				s.mu.Lock()
				s.client = nil
				up := s.connectedAt
				s.connectedAt = time.Time{}
				s.mu.Unlock()
				if closeErr := client.Close(); closeErr != nil {
					err = errors.Join(err, ErrIncompleteClose, closeErr)
					s.mu.Lock()
					s.closeError = errors.Join(s.closeError, closeErr)
					s.mu.Unlock()
				}
				if !up.IsZero() && time.Since(up) >= s.policy.MaxDelay {
					s.mu.Lock()
					s.snapshot.Attempts = 0
					s.mu.Unlock()
				}
			}
		}
		if s.ctx.Err() != nil {
			s.clear("closed", nil)
			return
		}
		s.changeStatus("disconnected", err)
		if errors.Is(err, ErrIncompleteClose) {
			s.changeStatus("blocked", err)
			s.log("transport closure unconfirmed; recovery stopped to avoid overlapping server processes")
			<-s.ctx.Done()
			s.clear("closed", err)
			return
		}
		if !*s.policy.Enabled {
			s.log("reconnect disabled; tools remain listed until reload")
			<-s.ctx.Done()
			s.clear("closed", nil)
			return
		}
		s.mu.Lock()
		s.snapshot.Attempts++
		attempt := s.snapshot.Attempts
		s.mu.Unlock()
		if attempt > s.policy.MaxAttempts {
			s.mu.Lock()
			s.snapshot.Attempts = s.policy.MaxAttempts
			s.mu.Unlock()
			s.clear("exhausted", err)
			s.log("reconnect attempts exhausted; tools unregistered until reload")
			<-s.ctx.Done()
			s.clear("closed", nil)
			return
		}
		delay := s.policy.InitialDelay
		for n := 1; n < attempt && delay < s.policy.MaxDelay; n++ {
			if delay > s.policy.MaxDelay/2 {
				delay = s.policy.MaxDelay
			} else {
				delay *= 2
			}
		}
		if delay > s.policy.MaxDelay {
			delay = s.policy.MaxDelay
		}
		s.log(fmt.Sprintf("retrying in %s (attempt %d/%d)", delay, attempt, s.policy.MaxAttempts))
		timer := time.NewTimer(delay)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			s.clear("closed", nil)
			return
		case <-timer.C:
		}
		err = s.connect()
		if err != nil {
			s.log("connection attempt failed: " + err.Error())
		}
	}
}

func (s *Supervisor) current() (*Client, discoveredTools, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil || s.client == nil || s.snapshot.Status != "connected" {
		return nil, discoveredTools{}, ErrDisconnected
	}
	return s.client, s.set, nil
}

func (s *Supervisor) CallTool(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
	client, set, err := s.current()
	if err != nil {
		return ToolResult{}, err
	}
	found := false
	for _, spec := range set.specs {
		if spec.Name == name {
			found = true
			break
		}
	}
	if !found {
		return ToolResult{}, errors.New("MCP tool is no longer registered")
	}
	return client.callToolChecked(withParamHeaders(ctx, set.paramHeaders[name], args), name, args, set.outputs[name], set.tasks[name], set.outputErrors[name])
}

func (s *Supervisor) Call(ctx context.Context, method string, params any, out any) error {
	client, _, err := s.current()
	if err != nil {
		return err
	}
	if method == "resources/read" {
		return client.readResourceParams(ctx, params, out)
	}
	return client.Call(ctx, method, params, out)
}

func (s *Supervisor) ListResources(ctx context.Context) ([]json.RawMessage, error) {
	client, _, err := s.current()
	if err != nil {
		return nil, err
	}
	return client.ListResources(ctx)
}

func (s *Supervisor) ListResourceTemplates(ctx context.Context) ([]json.RawMessage, error) {
	client, _, err := s.current()
	if err != nil {
		return nil, err
	}
	return client.ListResourceTemplates(ctx)
}

func (s *Supervisor) Close() error {
	s.closeOnce.Do(func() { s.cancel(); <-s.done })
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeError
}
