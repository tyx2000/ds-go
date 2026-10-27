package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"dsgo/internal/core"
	"dsgo/internal/mcp"
)

func (a *App) connectMCP() error {
	names := []string{}
	for name, cfg := range a.cfg.MCPServers {
		if !cfg.Disabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		client, err := mcp.Open(a.ctx, a.flags.cwd, a.cfg.MCPServers[name])
		if err != nil {
			return fmt.Errorf("MCP %s: %w", name, err)
		}
		a.mcpClients[name] = client
		specs, err := client.ListTools(a.ctx)
		if err != nil {
			return fmt.Errorf("MCP %s list tools: %w", name, err)
		}
		if client.Instructions != "" {
			a.mcpInstructions += "\nInstructions from configured MCP server " + name + ":\n" + client.Instructions + "\n"
		}
		for _, s := range specs {
			rawName := s.Name
			public := mcp.PublicToolName(name, rawName)
			s.Name = public
			s.Description = "MCP " + name + ": " + s.Description
			server, connection := name, client
			if err = a.registry.Register(s, func(ctx context.Context, tc core.ToolContext, input json.RawMessage) (core.ToolResult, error) {
				if err := a.mcpApprove(ctx, tc, server+"/"+rawName, input); err != nil {
					return core.ToolResult{}, err
				}
				result, err := connection.CallTool(ctx, rawName, input)
				if err != nil && len(result.Content) == 0 {
					return core.ToolResult{}, err
				}
				return core.ToolResult{Content: projectMCP(result), IsError: result.IsError || err != nil}, nil
			}); err != nil {
				return err
			}
		}
	}
	if len(names) > 0 {
		return a.registry.Register(toolSpec("mcp_resources", "List or read resources on an explicitly configured MCP server.", `{"type":"object","properties":{"server":{"type":"string"},"action":{"type":"string","enum":["list","templates","read"]},"uri":{"type":"string"}},"required":["server","action"]}`), func(ctx context.Context, tc core.ToolContext, input json.RawMessage) (core.ToolResult, error) {
			var p struct{ Server, Action, URI string }
			if err := json.Unmarshal(input, &p); err != nil {
				return core.ToolResult{}, err
			}
			client, ok := a.mcpClients[p.Server]
			if !ok {
				return core.ToolResult{}, errors.New("MCP server is not connected")
			}
			method := "resources/list"
			params := map[string]any{}
			switch p.Action {
			case "list":
			case "templates":
				method = "resources/templates/list"
			case "read":
				if p.URI == "" {
					return core.ToolResult{}, errors.New("resource uri required")
				}
				method = "resources/read"
				params["uri"] = p.URI
			default:
				return core.ToolResult{}, errors.New("unknown resource action")
			}
			if err := a.mcpApprove(ctx, tc, p.Server+"/"+method, input); err != nil {
				return core.ToolResult{}, err
			}
			if p.Action == "list" {
				values, err := client.ListResources(ctx)
				return core.ToolResult{Content: mcp.RenderResourceResult(p.Server, values)}, err
			}
			if p.Action == "templates" {
				values, err := client.ListResourceTemplates(ctx)
				return core.ToolResult{Content: mcp.RenderResourceResult(p.Server, values)}, err
			}
			var out json.RawMessage
			if err := client.Call(ctx, method, params, &out); err != nil {
				return core.ToolResult{}, err
			}
			return core.ToolResult{Content: mcp.RenderResourceResult(p.Server, out)}, nil
		})
	}
	return nil
}

func (a *App) mcpApprove(ctx context.Context, tc core.ToolContext, name string, input json.RawMessage) error {
	if tc.Permission == "full-access" {
		return ctx.Err()
	}
	if tc.ApprovalPolicy == "never" || tc.Approve == nil {
		return errors.New("MCP tool requires approval; interaction unavailable")
	}
	q := core.Approval{ID: name, SessionID: tc.SessionID, Tool: name, Description: "Run a tool supplied by an explicitly configured MCP server. Its effects are controlled by that server.", Input: input}
	if tc.Emit != nil {
		if err := tc.Emit("approval/requested", q); err != nil {
			return err
		}
	}
	ok, err := tc.Approve(ctx, q)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if tc.Emit != nil {
		if logErr := tc.Emit("approval/resolved", map[string]any{"id": q.ID, "allowed": ok && err == nil}); logErr != nil {
			return logErr
		}
	}
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("MCP tool approval denied")
	}
	return nil
}

func projectMCP(result mcp.ToolResult) string {
	var out []string
	for _, raw := range result.Content {
		var block struct {
			Type     string
			Text     string
			Name     string
			URI      string
			MimeType string `json:"mimeType"`
			Resource json.RawMessage
		}
		if err := json.Unmarshal(raw, &block); err != nil {
			out = append(out, "[invalid MCP content block]")
			continue
		}
		switch block.Type {
		case "text":
			out = append(out, block.Text)
		case "resource_link":
			out = append(out, fmt.Sprintf("Resource: %s (%s)", block.Name, block.URI))
		case "resource":
			out = append(out, mcp.RenderResourceResult("embedded", block.Resource))
		case "image", "audio":
			out = append(out, fmt.Sprintf("[MCP %s (%s): binary attachment rendering is not implemented]", block.Type, block.MimeType))
		default:
			out = append(out, "[unsupported MCP content type: "+block.Type+"]")
		}
	}
	if len(result.StructuredContent) > 0 {
		out = append(out, "Structured result: "+string(result.StructuredContent))
	}
	return strings.Join(out, "\n")
}
