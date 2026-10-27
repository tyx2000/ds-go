package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"dsgo/internal/attachments"
	"dsgo/internal/core"
	"dsgo/internal/mcp"
)

type mcpContent struct {
	Type, Text, Data, Name, URI string
	MimeType                    string `json:"mimeType"`
}

// projectMCPResult retains the complete raw value only for this execution's
// programmatic caller. Durable history receives text and verified image refs.
func (a *App) projectMCPResult(ctx context.Context, tc core.ToolContext, name string, result mcp.ToolResult) core.ToolResult {
	decoded := make([]mcpContent, len(result.Content))
	indexes := []int{}
	inputs := []attachments.ImageInput{}
	refusals := map[int]string{}
	for i, raw := range result.Content {
		if json.Unmarshal(raw, &decoded[i]) != nil {
			refusals[i] = "invalid MCP content block"
			continue
		}
		block := decoded[i]
		if block.Type != "image" {
			continue
		}
		indexes = append(indexes, i)
		mime := block.MimeType
		if mime != "image/png" && mime != "image/jpeg" && mime != "image/webp" && mime != "image/gif" {
			refusals[i] = "the declared media type is not PNG, JPEG, WebP, or GIF"
			continue
		}
		data, err := base64.StdEncoding.Strict().DecodeString(block.Data)
		if err != nil || base64.StdEncoding.EncodeToString(data) != block.Data {
			refusals[i] = "the image data is not canonical base64"
			continue
		}
		inputs = append(inputs, attachments.ImageInput{Data: data, MediaType: mime})
	}
	var refs []attachments.ImageRef
	if len(indexes) > 0 && !result.IsError {
		if len(refusals) > 0 {
			for _, i := range indexes {
				if refusals[i] == "" {
					refusals[i] = "another image in the same result was invalid"
				}
			}
		} else {
			var err error
			if a.attachmentStore == nil {
				err = fmt.Errorf("no attachment store is mounted")
			} else {
				err = a.assertImageRoute(ctx, tc.SessionID, name)
			}
			if err == nil {
				refs, err = a.attachmentStore.SaveImages(ctx, inputs)
			}
			if err != nil {
				for _, i := range indexes {
					refusals[i] = err.Error()
				}
			}
		}
	}
	blocks := []core.Block{}
	text := []string{}
	flush := func() {
		if len(text) > 0 {
			blocks = append(blocks, core.Block{Type: "text", Text: strings.Join(text, "\n")})
			text = nil
		}
	}
	refIndex := 0
	for i, block := range decoded {
		switch block.Type {
		case "text":
			text = append(text, block.Text)
		case "image":
			flush()
			reason := refusals[i]
			if result.IsError {
				reason = "the MCP tool returned an error"
			}
			if reason != "" || refIndex >= len(refs) {
				if reason == "" {
					reason = "this result was not admitted to durable model context"
				}
				blocks = append(blocks, core.Block{Type: "text", Text: fmt.Sprintf("[image unavailable: %s; %s; raw image data remains available to programmatic callers]", block.MimeType, reason)})
			} else {
				blocks = append(blocks, core.Block{Type: "image", Source: refs[refIndex].Source()})
				refIndex++
			}
		case "resource_link":
			text = append(text, fmt.Sprintf("Resource link: %s (%s)", block.Name, block.URI))
		case "audio":
			text = append(text, fmt.Sprintf("[audio result unsupported: %s; raw audio data remains available to programmatic callers]", block.MimeType))
		case "resource":
			text = append(text, "[embedded resource unsupported; raw resource data remains available to programmatic callers]")
		default:
			text = append(text, "[unsupported MCP content type: "+block.Type+"]")
		}
	}
	flush()
	if len(blocks) == 0 {
		blocks = append(blocks, core.Block{Type: "text", Text: "(" + name + " returned no model-visible content)"})
	}
	var visible []string
	for _, block := range blocks {
		if block.Type == "text" {
			visible = append(visible, block.Text)
		}
	}
	out := core.ToolResult{Content: strings.Join(visible, "\n"), IsError: result.IsError}
	if !result.IsError {
		out.Value = core.JSON(struct {
			Content    []json.RawMessage `json:"content"`
			Structured json.RawMessage   `json:"structuredContent,omitempty"`
		}{result.Content, result.StructuredContent})
	}
	if len(indexes) > 0 {
		out.Blocks = blocks
	}
	return out
}
