package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"dsgo/internal/core"
	"dsgo/internal/mcp"
)

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
