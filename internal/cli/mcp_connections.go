package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"dsgo/internal/core"
	"dsgo/internal/mcp"
	"dsgo/internal/state"
	"dsgo/internal/tools"
)

func (a *App) mcpSystemInstructions() string {
	a.mcpMu.RLock()
	defer a.mcpMu.RUnlock()
	names := make([]string, 0, len(a.mcpInstructions))
	for name, text := range a.mcpInstructions {
		if text != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var text strings.Builder
	for _, name := range names {
		text.WriteString("\n\n")
		text.WriteString(a.mcpInstructions[name])
	}
	return text.String()
}

func (a *App) mcpConnection(name string) (*mcp.Supervisor, error) {
	a.mcpMu.RLock()
	connection := a.mcpSupervisors[name]
	a.mcpMu.RUnlock()
	if connection == nil {
		return nil, errors.New("MCP server is not connected")
	}
	return connection, nil
}

func (a *App) publishMCP(snapshot mcp.SupervisorSnapshot) error {
	entries := make([]tools.Registration, 0, len(snapshot.Tools))
	for _, spec := range snapshot.Tools {
		rawName, server := spec.Name, snapshot.Name
		spec.Name = mcp.PublicToolName(server, rawName)
		entries = append(entries, tools.Registration{Spec: spec, InputValidatedByServer: true, Run: func(ctx context.Context, tc core.ToolContext, input json.RawMessage) (core.ToolResult, error) {
			if err := a.mcpApprove(ctx, tc, server+"/"+rawName, input); err != nil {
				return core.ToolResult{}, err
			}
			connection, err := a.mcpConnection(server)
			if err != nil {
				return core.ToolResult{}, err
			}
			result, err := connection.CallTool(ctx, rawName, input)
			if err != nil && len(result.Content) == 0 {
				return core.ToolResult{}, err
			}
			result.IsError = result.IsError || err != nil
			return a.projectMCPResult(ctx, tc, rawName, result), nil
		}})
	}
	if err := a.registry.ReplaceGroup("mcp:"+snapshot.Name, entries); err != nil {
		return err
	}
	a.mcpMu.Lock()
	if a.mcpInstructions == nil {
		a.mcpInstructions = map[string]string{}
	}
	if snapshot.Instructions == "" {
		delete(a.mcpInstructions, snapshot.Name)
	} else {
		a.mcpInstructions[snapshot.Name] = snapshot.Instructions
	}
	a.mcpMu.Unlock()
	return nil
}

func (a *App) mcpLog(message string) {
	a.errorLine(clean(message))
}

func (a *App) connectMCP() error {
	a.mcpContext, a.mcpCancel = context.WithCancel(a.ctx)
	if err := a.reloadMCP(a.cfg.MCPServers); err != nil {
		return err
	}
	// The JSON configuration editor runs in a separate invocation. Reload only
	// MCP entries here; runtime model settings retain their existing ownership.
	if a.flags.home != "" {
		a.mcpWatchDone = make(chan struct{})
		go a.watchMCPConfig()
	}
	return nil
}

func (a *App) reloadMCP(configured map[string]state.MCPServer) error {
	a.mcpReloadMu.Lock()
	defer a.mcpReloadMu.Unlock()
	if err := a.mcpContext.Err(); err != nil {
		return err
	}
	a.mcpMu.Lock()
	if a.mcpSupervisors == nil {
		a.mcpSupervisors = map[string]*mcp.Supervisor{}
	}
	if a.mcpEntries == nil {
		a.mcpEntries = map[string]state.MCPServer{}
	}
	names := map[string]bool{}
	for name := range a.mcpEntries {
		names[name] = true
	}
	for name := range configured {
		names[name] = true
	}
	a.mcpMu.Unlock()
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	for _, name := range ordered {
		next, exists := configured[name]
		a.mcpMu.RLock()
		old, had := a.mcpEntries[name]
		previous := a.mcpSupervisors[name]
		a.mcpMu.RUnlock()
		if had && exists && reflect.DeepEqual(old, next) {
			continue
		}
		if previous != nil {
			if err := previous.Close(); err != nil {
				return fmt.Errorf("close MCP %s before reload: %w", name, err)
			}
		}
		a.mcpMu.Lock()
		delete(a.mcpSupervisors, name)
		delete(a.mcpEntries, name)
		a.mcpMu.Unlock()
		if !exists {
			continue
		}
		a.mcpMu.Lock()
		a.mcpEntries[name] = next
		a.mcpMu.Unlock()
		if next.Disabled {
			continue
		}
		options := mcp.SupervisorOptions{Name: name, CWD: a.flags.cwd, Server: next, Publish: a.publishMCP, Log: a.mcpLog, FailOnStartupError: next.FailOnStartupError}
		if next.ToolCallTimeoutMS != nil {
			options.ToolCallTimeout = time.Duration(*next.ToolCallTimeoutMS) * time.Millisecond
		}
		if next.MaxInstructionBytes != nil {
			options.MaxInstructionBytes = *next.MaxInstructionBytes
		}
		if p := next.Reconnect; p != nil {
			options.Reconnect = &mcp.ReconnectPolicy{Enabled: p.Enabled}
			if p.InitialDelayMS != nil {
				options.Reconnect.InitialDelay = time.Duration(*p.InitialDelayMS) * time.Millisecond
			}
			if p.MaxDelayMS != nil {
				options.Reconnect.MaxDelay = time.Duration(*p.MaxDelayMS) * time.Millisecond
			}
			if p.MaxAttempts != nil {
				options.Reconnect.MaxAttempts = *p.MaxAttempts
			}
		}
		connection, err := mcp.OpenSupervisor(a.mcpContext, options)
		if err != nil {
			return fmt.Errorf("MCP %s: %w", name, err)
		}
		a.mcpMu.Lock()
		a.mcpSupervisors[name] = connection
		a.mcpMu.Unlock()
	}
	enabled := false
	for _, entry := range configured {
		enabled = enabled || !entry.Disabled
	}
	return a.registerMCPResources(enabled)
}

func (a *App) watchMCPConfig() {
	defer close(a.mcpWatchDone)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	lastError := ""
	for {
		select {
		case <-a.mcpContext.Done():
			return
		case <-ticker.C:
			cfg, err := state.LoadConfig(a.flags.home)
			if err == nil {
				err = a.reloadMCP(cfg.MCPServers)
			}
			if err != nil && a.mcpContext.Err() == nil {
				if err.Error() != lastError {
					a.mcpLog("MCP configuration reload: " + err.Error())
				}
				lastError = err.Error()
			} else {
				lastError = ""
			}
		}
	}
}

func (a *App) closeMCP() {
	if a.mcpCancel != nil {
		a.mcpCancel()
	}
	if a.mcpWatchDone != nil {
		<-a.mcpWatchDone
	}
	a.mcpReloadMu.Lock()
	defer a.mcpReloadMu.Unlock()
	a.mcpMu.RLock()
	connections := make([]*mcp.Supervisor, 0, len(a.mcpSupervisors))
	for _, connection := range a.mcpSupervisors {
		connections = append(connections, connection)
	}
	a.mcpMu.RUnlock()
	for _, connection := range connections {
		if err := connection.Close(); err != nil {
			a.mcpLog("MCP close: " + err.Error())
		}
	}
}

func (a *App) registerMCPResources(enabled bool) error {
	entries := []tools.Registration{}
	if enabled {
		entries = append(entries, tools.Registration{Spec: toolSpec("mcp_resources", "List or read resources on an explicitly configured MCP server.", `{"type":"object","properties":{"server":{"type":"string"},"action":{"type":"string","enum":["list","templates","read"]},"uri":{"type":"string"}},"required":["server","action"]}`), Run: func(ctx context.Context, tc core.ToolContext, input json.RawMessage) (core.ToolResult, error) {
			var p struct{ Server, Action, URI string }
			if err := json.Unmarshal(input, &p); err != nil {
				return core.ToolResult{}, err
			}
			connection, err := a.mcpConnection(p.Server)
			if err != nil {
				return core.ToolResult{}, err
			}
			method, params := "resources/list", map[string]any{}
			switch p.Action {
			case "list":
			case "templates":
				method = "resources/templates/list"
			case "read":
				if p.URI == "" {
					return core.ToolResult{}, errors.New("resource uri required")
				}
				method, params["uri"] = "resources/read", p.URI
			default:
				return core.ToolResult{}, errors.New("unknown resource action")
			}
			if err := a.mcpApprove(ctx, tc, p.Server+"/"+method, input); err != nil {
				return core.ToolResult{}, err
			}
			if p.Action == "list" {
				values, err := connection.ListResources(ctx)
				return core.ToolResult{Content: mcp.RenderResourceResult(p.Server, values)}, err
			}
			if p.Action == "templates" {
				values, err := connection.ListResourceTemplates(ctx)
				return core.ToolResult{Content: mcp.RenderResourceResult(p.Server, values)}, err
			}
			var out json.RawMessage
			if err := connection.Call(ctx, method, params, &out); err != nil {
				return core.ToolResult{}, err
			}
			return core.ToolResult{Content: mcp.RenderResourceResult(p.Server, out)}, nil
		}})
	}
	return a.registry.ReplaceGroup("mcp-resources", entries)
}
