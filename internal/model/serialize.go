package model

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"strings"

	"dsgo/internal/core"
)

const (
	maxRequestBytes     = 192 << 20
	maxResponseBytes    = 64 << 20
	maxEventBytes       = 8 << 20
	maxInlineImageBytes = 20 << 20 // Includes base64 expansion, as on the upstream fallback route.
	maxImages           = 600
)

type wireRequest struct {
	Model         string            `json:"model"`
	Stream        bool              `json:"stream"`
	MaxTokens     int               `json:"max_tokens"`
	System        string            `json:"system,omitempty"`
	Thinking      map[string]string `json:"thinking"`
	OutputConfig  map[string]string `json:"output_config,omitempty"`
	Messages      []core.Message    `json:"messages"`
	Tools         []core.ToolSpec   `json:"tools,omitempty"`
	Temperature   *float64          `json:"temperature,omitempty"`
	StopSequences []string          `json:"stop_sequences,omitempty"`
}

// core.Block uses omitempty for storage. Messages requires even an empty
// thinking string to remain present when replaying a signed thinking block.
func (w wireRequest) MarshalJSON() ([]byte, error) {
	type alias wireRequest
	type message struct {
		Role    string `json:"role"`
		Content []any  `json:"content"`
	}
	messages := make([]message, 0, len(w.Messages))
	for _, source := range w.Messages {
		out := message{Role: source.Role, Content: make([]any, 0, len(source.Content))}
		for _, block := range source.Content {
			if block.Type == "thinking" {
				value := map[string]string{"type": "thinking", "thinking": block.Thinking}
				if block.Signature != "" {
					value["signature"] = block.Signature
				}
				out.Content = append(out.Content, value)
			} else if block.Type == "tool_addition" || block.Type == "tool_removal" {
				out.Content = append(out.Content, map[string]any{"type": block.Type, "tool": map[string]string{"type": "tool_reference", "name": block.ToolName}})
			} else {
				out.Content = append(out.Content, block)
			}
		}
		messages = append(messages, out)
	}
	return json.Marshal(struct {
		*alias
		Messages []message `json:"messages"`
	}{alias: (*alias)(&w), Messages: messages})
}

func invalid() error { return &Error{Code: "INVALID_REQUEST"} }

func serialize(input core.Request) (wireRequest, error) {
	wire := wireRequest{Model: input.Model, Stream: true, MaxTokens: input.MaxTokens, Tools: input.Tools, Messages: []core.Message{}, Temperature: input.Temperature, StopSequences: input.StopSequences}
	if input.Temperature != nil && (math.IsNaN(*input.Temperature) || math.IsInf(*input.Temperature, 0)) {
		return wire, invalid()
	}
	if input.SystemPromptUpdate != "" && input.SystemPromptUpdate != "in-history" {
		return wire, invalid()
	}
	if input.ToolUpdate != "" && input.ToolUpdate != "addition-only" && input.ToolUpdate != "in-history" {
		return wire, invalid()
	}
	for _, value := range []string{input.UserID, input.SessionID} {
		if len(value) > 512 || strings.ContainsAny(value, "\r\n\x00") {
			return wire, invalid()
		}
	}
	if strings.TrimSpace(wire.Model) == "" {
		return wire, invalid()
	}
	if wire.MaxTokens == 0 {
		wire.MaxTokens = 256000
	}
	if wire.MaxTokens < 1 {
		return wire, invalid()
	}
	effort := input.Thinking
	if effort == "" {
		effort = "high"
	}
	if effort != "off" && effort != "low" && effort != "high" && effort != "max" {
		return wire, &Error{Code: "UNSUPPORTED_REASONING_EFFORT"}
	}
	wire.Thinking = map[string]string{"type": "enabled"}
	if effort == "off" {
		wire.Thinking["type"] = "disabled"
	} else {
		wire.OutputConfig = map[string]string{"effort": effort}
	}
	declared := map[string]bool{}
	for _, tool := range wire.Tools {
		if tool.Name == "" || !jsonObject(tool.InputSchema) {
			return wire, invalid()
		}
		if declared[tool.Name] {
			return wire, invalid()
		}
		declared[tool.Name] = true
	}
	historySystem := ""
	var updates []core.Message
	flushUpdates := func() error {
		if len(updates) == 0 {
			return nil
		}
		if len(wire.Messages) == 0 || wire.Messages[len(wire.Messages)-1].Role != "user" {
			return &Error{Code: "UNSUPPORTED_CONTENT"}
		}
		wire.Messages = append(wire.Messages, updates...)
		updates = nil
		return nil
	}
	for _, message := range input.Messages {
		if message.Role == "developer" {
			out := core.Message{Role: "system", Content: []core.Block{}}
			for _, b := range message.Content {
				switch b.Type {
				case "text":
					if b.Text != "" {
						out.Content = append(out.Content, core.Block{Type: "text", Text: b.Text})
					}
				case "tool-addition", "tool_addition", "tool-removal", "tool_removal":
					if input.ToolUpdate == "" || b.ToolName == "" || !declared[b.ToolName] {
						return wire, &Error{Code: "UNSUPPORTED_CONTENT"}
					}
					kind := strings.ReplaceAll(b.Type, "-", "_")
					if kind == "tool_removal" && input.ToolUpdate == "addition-only" {
						return wire, &Error{Code: "UNSUPPORTED_CONTENT"}
					}
					out.Content = append(out.Content, core.Block{Type: kind, ToolName: b.ToolName})
				default:
					return wire, &Error{Code: "UNSUPPORTED_CONTENT"}
				}
			}
			if len(out.Content) > 0 {
				updates = append(updates, out)
			}
			continue
		}
		if message.Role == "system" {
			var text strings.Builder
			for _, block := range message.Content {
				if block.Type != "text" {
					return wire, invalid()
				}
				text.WriteString(block.Text)
			}
			if input.SystemPromptUpdate == "in-history" && len(wire.Messages) > 0 {
				if text.Len() == 0 {
					return wire, &Error{Code: "UNSUPPORTED_CONTENT"}
				}
				updates = append(updates, core.TextMessage("system", text.String()))
			} else {
				historySystem = text.String()
			}
			continue
		}
		if message.Role != "assistant" && message.Role != "user" {
			return wire, &Error{Code: "UNSUPPORTED_CONTENT"}
		}
		out := core.Message{Role: message.Role, Content: []core.Block{}}
		for _, block := range message.Content {
			if message.Role == "assistant" {
				switch block.Type {
				case "text":
					if block.Text != "" {
						out.Content = append(out.Content, core.Block{Type: "text", Text: block.Text})
					}
				case "thinking", "reasoning":
					thought := block.Thinking
					if block.Type == "reasoning" {
						thought = block.Text
					}
					signature := block.Signature
					source := block.Source
					if !replayMatches(message, input) {
						signature = ""
						source = nil
					}
					out.Content = append(out.Content, core.Block{Type: "thinking", Thinking: thought, Signature: signature, Source: source})
				case "redacted_thinking":
					if replayMatches(message, input) && block.Data != "" {
						out.Content = append(out.Content, core.Block{Type: "redacted_thinking", Data: block.Data})
					}
				case "tool_use":
					if block.ID == "" || block.Name == "" {
						return wire, invalid()
					}
					args := append(json.RawMessage(nil), block.Input...)
					if !jsonObject(args) {
						args = json.RawMessage(`{}`)
					}
					out.Content = append(out.Content, core.Block{Type: "tool_use", ID: block.ID, Name: block.Name, Input: args})
				default:
					return wire, &Error{Code: "UNSUPPORTED_CONTENT"}
				}
			} else {
				if block.Type == "tool_result" {
					if block.ToolUseID == "" {
						return wire, invalid()
					}
					parts, err := toolContent(block.Content)
					if err != nil {
						return wire, err
					}
					out.Content = append(out.Content, core.Block{Type: "tool_result", ToolUseID: block.ToolUseID, Content: parts, IsError: block.IsError})
				} else {
					part, keep, err := inputBlock(block)
					if err != nil {
						return wire, err
					}
					if keep {
						out.Content = append(out.Content, part)
					}
				}
			}
		}
		if len(out.Content) == 0 {
			continue
		}
		if out.Role == "assistant" {
			if err := flushUpdates(); err != nil {
				return wire, err
			}
		}
		if len(wire.Messages) > 0 && wire.Messages[len(wire.Messages)-1].Role == out.Role {
			last := &wire.Messages[len(wire.Messages)-1]
			last.Content = append(last.Content, out.Content...)
		} else {
			wire.Messages = append(wire.Messages, out)
		}
	}
	if err := flushUpdates(); err != nil {
		return wire, err
	}
	wire.System = input.System
	if historySystem != "" {
		if wire.System != "" {
			wire.System += "\n\n"
		}
		wire.System += historySystem
	}
	if len(wire.Messages) == 0 {
		return wire, invalid()
	}
	// Results must immediately follow their calls and are ordered before text.
	pending := map[string]bool{}
	for i := range wire.Messages {
		message := &wire.Messages[i]
		if message.Role == "assistant" {
			if len(pending) > 0 {
				return wire, invalid()
			}
			for _, block := range message.Content {
				if block.Type == "tool_use" {
					if pending[block.ID] {
						return wire, invalid()
					}
					pending[block.ID] = true
				}
			}
		} else if message.Role == "user" {
			ordered := make([]core.Block, 0, len(message.Content))
			for _, block := range message.Content {
				if block.Type == "tool_result" {
					if !pending[block.ToolUseID] {
						return wire, invalid()
					}
					delete(pending, block.ToolUseID)
					ordered = append(ordered, block)
				}
			}
			if len(pending) > 0 {
				return wire, invalid()
			}
			for _, block := range message.Content {
				if block.Type != "tool_result" {
					ordered = append(ordered, block)
				}
			}
			message.Content = ordered
		}
	}
	if len(pending) > 0 {
		return wire, invalid()
	}
	if err := imageBlocks(&wire, func(*core.Block) error { return nil }); err != nil {
		return wire, err
	}
	return wire, nil
}

func jsonObject(data []byte) bool {
	data = []byte(strings.TrimSpace(string(data)))
	return len(data) > 0 && data[0] == '{' && json.Valid(data)
}

func replayMatches(message core.Message, input core.Request) bool {
	// A provider name can be rebound to a different endpoint or credential.
	// Missing provenance is foreign, including logs written before this field.
	if !sameReplayIdentity(message.ReplayIdentity, input.ReplayIdentity) {
		return false
	}
	if message.ReplayModel != "" && message.ReplayModel != input.Model {
		return false
	}
	if message.ReplayProvider != "" && input.Provider != "" && message.ReplayProvider != input.Provider {
		return false
	}
	return true
}

func inputBlock(block core.Block) (core.Block, bool, error) {
	switch block.Type {
	case "text":
		return core.Block{Type: "text", Text: block.Text}, block.Text != "", nil
	case "thinking", "reasoning", "tool_use":
		return core.Block{}, false, nil
	case "image":
		data, err := json.Marshal(block.Source)
		if err != nil || len(data) > maxRequestBytes {
			return core.Block{}, false, invalid()
		}
		var source map[string]string
		if json.Unmarshal(data, &source) != nil {
			return core.Block{}, false, invalid()
		}
		switch source["type"] {
		case "base64":
			media := source["media_type"]
			if media != "image/png" && media != "image/jpeg" && media != "image/gif" && media != "image/webp" {
				return core.Block{}, false, invalid()
			}
			if base64.StdEncoding.DecodedLen(len(source["data"])) > maxUploadBytes {
				return core.Block{}, false, &Error{Code: "IMAGE_LIMIT"}
			}
			bytes, err := base64.StdEncoding.DecodeString(source["data"])
			if err != nil || len(bytes) == 0 {
				return core.Block{}, false, invalid()
			}
			return core.Block{Type: "image", Source: map[string]string{"type": "base64", "media_type": media, "data": source["data"]}}, true, nil
		case "file":
			if source["file_id"] == "" {
				return core.Block{}, false, invalid()
			}
			return core.Block{Type: "image", Source: map[string]string{"type": "file", "file_id": source["file_id"]}}, true, nil
		default:
			return core.Block{}, false, &Error{Code: "UNSUPPORTED_CONTENT"}
		}
	default:
		return core.Block{}, false, &Error{Code: "UNSUPPORTED_CONTENT"}
	}
}

func toolContent(content any) ([]core.Block, error) {
	if content == nil {
		return []core.Block{}, nil
	}
	if text, ok := content.(string); ok {
		if text == "" {
			return []core.Block{}, nil
		}
		return []core.Block{{Type: "text", Text: text}}, nil
	}
	data, err := json.Marshal(content)
	if err != nil {
		return nil, invalid()
	}
	var blocks []core.Block
	if json.Unmarshal(data, &blocks) != nil {
		return nil, invalid()
	}
	out := []core.Block{}
	for _, block := range blocks {
		part, keep, err := inputBlock(block)
		if err != nil {
			return nil, err
		}
		if keep {
			out = append(out, part)
		}
	}
	return out, nil
}

func cloneWireRequest(w wireRequest) wireRequest {
	type plain wireRequest
	data, _ := json.Marshal(plain(w))
	var cloned wireRequest
	_ = json.Unmarshal(data, &cloned)
	return cloned
}

func imageBlocks(w *wireRequest, visit func(*core.Block) error) error {
	count := 0
	for i := range w.Messages {
		for j := range w.Messages[i].Content {
			block := &w.Messages[i].Content[j]
			if block.Type == "image" {
				count++
				if err := visit(block); err != nil {
					return err
				}
			}
			if block.Type == "tool_result" {
				parts, err := toolContent(block.Content)
				if err != nil {
					return err
				}
				for k := range parts {
					if parts[k].Type == "image" {
						count++
						if err := visit(&parts[k]); err != nil {
							return err
						}
					}
				}
				block.Content = parts
			}
		}
	}
	if count > maxImages {
		return &Error{Code: "IMAGE_LIMIT"}
	}
	return nil
}

func sourceOf(b *core.Block) map[string]string {
	data, _ := json.Marshal(b.Source)
	var source map[string]string
	_ = json.Unmarshal(data, &source)
	return source
}
func containsFile(w wireRequest) bool {
	found := false
	_ = imageBlocks(&w, func(b *core.Block) error { found = found || sourceOf(b)["type"] == "file"; return nil })
	return found
}
func imageBytes(w wireRequest) int {
	n := 0
	_ = imageBlocks(&w, func(b *core.Block) error { n += len(sourceOf(b)["data"]); return nil })
	return n
}
