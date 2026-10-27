package model

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"dsgo/internal/core"
)

type openAIUsage struct {
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
	InputTokens      *int `json:"input_tokens"`
	OutputTokens     *int `json:"output_tokens"`
	PromptDetails    struct {
		CachedTokens *int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputDetails struct {
		CachedTokens *int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

func openAIUsageOf(raw json.RawMessage) (core.Usage, error) {
	var usage core.Usage
	if len(raw) == 0 || string(raw) == "null" {
		return usage, nil
	}
	var wire openAIUsage
	if json.Unmarshal(raw, &wire) != nil {
		return usage, malformed()
	}
	for _, value := range []*int{wire.PromptTokens, wire.CompletionTokens, wire.InputTokens, wire.OutputTokens, wire.PromptDetails.CachedTokens, wire.InputDetails.CachedTokens} {
		if value != nil && *value < 0 {
			return usage, malformed()
		}
	}
	input := wire.InputTokens
	if input == nil {
		input = wire.PromptTokens
	}
	output := wire.OutputTokens
	if output == nil {
		output = wire.CompletionTokens
	}
	cached := wire.InputDetails.CachedTokens
	if cached == nil {
		cached = wire.PromptDetails.CachedTokens
	}
	if input != nil {
		usage.InputTokens = *input
	}
	if output != nil {
		usage.OutputTokens = *output
	}
	if cached != nil {
		usage.CacheReadTokens = *cached
		if *cached > usage.InputTokens {
			return usage, malformed()
		}
		usage.InputTokens -= *cached
	}
	return usage, nil
}

type chatTool struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type chatContent struct {
	Content          *string    `json:"content"`
	ReasoningContent *string    `json:"reasoning_content"`
	Reasoning        *string    `json:"reasoning"`
	Refusal          *string    `json:"refusal"`
	ToolCalls        []chatTool `json:"tool_calls"`
}
type chatChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Index        int         `json:"index"`
		Delta        chatContent `json:"delta"`
		Message      chatContent `json:"message"`
		FinishReason *string     `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
	Error json.RawMessage `json:"error"`
}

func chatStop(value string) (string, error) {
	switch value {
	case "stop":
		return "end_turn", nil
	case "tool_calls", "function_call":
		return "tool_use", nil
	case "length":
		return "max_tokens", nil
	case "content_filter":
		return "stop_sequence", nil
	}
	return "", malformed()
}

func parseChat(reader io.Reader, isJSON bool, emit func(core.Delta)) (core.Response, bool, error) {
	response := core.Response{}
	indices := map[string]int{}
	started := false
	appendText := func(key, kind, text string) {
		if text == "" {
			return
		}
		index, exists := indices[key]
		if !exists {
			index = len(response.Content)
			indices[key] = index
			response.Content = append(response.Content, core.Block{Type: kind})
			emit(core.Delta{Type: "block_start", Index: index, Text: kind})
		}
		if kind == "text" {
			response.Content[index].Text += text
			emit(core.Delta{Type: "text", Index: index, Text: text})
		} else {
			response.Content[index].Thinking += text
			emit(core.Delta{Type: "thinking", Index: index, Text: text})
		}
		started = true
	}
	consume := func(chunk chatChunk, full bool) error {
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			return &Error{Code: "SERVER"}
		}
		if chunk.ID != "" {
			if response.ID != "" && response.ID != chunk.ID {
				return malformed()
			}
			response.ID = chunk.ID
		}
		if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
			usage, err := openAIUsageOf(chunk.Usage)
			if err != nil {
				return err
			}
			response.Usage = usage
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				continue
			}
			content := choice.Delta
			if full {
				content = choice.Message
			}
			if content.ReasoningContent != nil {
				appendText("thought", "thinking", *content.ReasoningContent)
			} else if content.Reasoning != nil {
				appendText("thought", "thinking", *content.Reasoning)
			}
			if content.Content != nil {
				appendText("text", "text", *content.Content)
			}
			if content.Refusal != nil {
				appendText("refusal", "text", *content.Refusal)
			}
			for n, tool := range content.ToolCalls {
				if tool.Type != "" && tool.Type != "function" {
					return &Error{Code: "UNSUPPORTED_CONTENT"}
				}
				wireIndex := tool.Index
				if full {
					wireIndex = n
				}
				if wireIndex < 0 || wireIndex >= 4096 {
					return malformed()
				}
				key := fmt.Sprintf("tool:%d", wireIndex)
				index, exists := indices[key]
				if !exists {
					if len(response.Content) >= 4096 {
						return malformed()
					}
					index = len(response.Content)
					indices[key] = index
					response.Content = append(response.Content, core.Block{Type: "tool_use"})
					emit(core.Delta{Type: "block_start", Index: index, Text: "tool_use"})
				}
				started = true
				b := &response.Content[index]
				b.ID += tool.ID
				b.Name += tool.Function.Name
				b.Data += tool.Function.Arguments
				if tool.Function.Arguments != "" {
					emit(core.Delta{Type: "tool_input", Index: index, Text: tool.Function.Arguments})
				}
			}
			if choice.FinishReason != nil {
				reason, err := chatStop(*choice.FinishReason)
				if err != nil {
					return err
				}
				if response.StopReason != "" && response.StopReason != reason {
					return malformed()
				}
				response.StopReason = reason
			}
		}
		return nil
	}
	settle := func() error {
		if response.ID == "" || response.StopReason == "" {
			return malformed()
		}
		for i := range response.Content {
			b := &response.Content[i]
			if b.Type == "tool_use" {
				if b.ID == "" || b.Name == "" {
					return malformed()
				}
				if jsonObject([]byte(b.Data)) {
					b.Input = json.RawMessage(b.Data)
					b.Data = ""
				} else if b.Data == "" {
					b.Input = json.RawMessage(`{}`)
				}
			}
		}
		if err := finalize(&response); err != nil {
			return err
		}
		for i := range response.Content {
			emit(core.Delta{Type: "block_stop", Index: i})
		}
		return nil
	}
	if isJSON {
		data, err := boundedJSON(reader)
		if err != nil {
			return response, false, err
		}
		var chunk chatChunk
		if !utf8.Valid(data) || json.Unmarshal(data, &chunk) != nil {
			return response, false, malformed()
		}
		if err := consume(chunk, true); err != nil {
			return response, started, err
		}
		err = settle()
		return response, started, err
	}
	err := consumeSSE(reader, func(_ string, frame string) (bool, error) {
		if frame == "[DONE]" {
			return true, settle()
		}
		if response.StopReason != "" {
			var trailing chatChunk
			if json.Unmarshal([]byte(frame), &trailing) != nil {
				return false, malformed()
			}
			if len(trailing.Choices) > 0 {
				return false, malformed()
			}
			return false, consume(trailing, false)
		}
		var chunk chatChunk
		if !utf8.ValidString(frame) || json.Unmarshal([]byte(frame), &chunk) != nil {
			return false, malformed()
		}
		return false, consume(chunk, false)
	})
	return response, started, err
}

type responseItem struct {
	Type             string `json:"type"`
	ID               string `json:"id"`
	CallID           string `json:"call_id"`
	Name             string `json:"name"`
	Arguments        string `json:"arguments"`
	EncryptedContent string `json:"encrypted_content"`
	Content          []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
	Summary []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"summary"`
}
type responsesEnvelope struct {
	ID                string            `json:"id"`
	Status            string            `json:"status"`
	Output            []json.RawMessage `json:"output"`
	Usage             json.RawMessage   `json:"usage"`
	IncompleteDetails struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
}

func responseBlocks(data json.RawMessage) ([]core.Block, error) {
	var item responseItem
	if json.Unmarshal(data, &item) != nil {
		return nil, malformed()
	}
	switch item.Type {
	case "message":
		out := []core.Block{}
		for _, part := range item.Content {
			switch part.Type {
			case "output_text":
				out = append(out, core.Block{Type: "text", Text: part.Text})
			case "refusal":
				out = append(out, core.Block{Type: "text", Text: part.Refusal})
			default:
				return nil, &Error{Code: "UNSUPPORTED_CONTENT"}
			}
		}
		return out, nil
	case "function_call":
		if item.CallID == "" || item.Name == "" {
			return nil, malformed()
		}
		b := core.Block{Type: "tool_use", ID: item.CallID, Name: item.Name}
		if jsonObject([]byte(item.Arguments)) {
			b.Input = json.RawMessage(item.Arguments)
		} else {
			b.Data = item.Arguments
		}
		return []core.Block{b}, nil
	case "reasoning":
		var source map[string]any
		_ = json.Unmarshal(data, &source)
		text := ""
		for _, part := range item.Summary {
			if part.Type != "summary_text" {
				return nil, &Error{Code: "UNSUPPORTED_CONTENT"}
			}
			text += part.Text
		}
		return []core.Block{{Type: "thinking", Thinking: text, Source: source}}, nil
	default:
		return nil, &Error{Code: "UNSUPPORTED_CONTENT"}
	}
}

func responsesFull(raw json.RawMessage) (core.Response, error) {
	var envelope responsesEnvelope
	if json.Unmarshal(raw, &envelope) != nil || envelope.ID == "" {
		return core.Response{}, malformed()
	}
	response := core.Response{ID: envelope.ID, StopReason: "end_turn"}
	if envelope.Status == "incomplete" {
		if envelope.IncompleteDetails.Reason == "max_output_tokens" {
			response.StopReason = "max_tokens"
		} else {
			return response, &Error{Code: "INCOMPLETE_RESPONSE"}
		}
	} else if envelope.Status != "completed" {
		return response, &Error{Code: "SERVER"}
	}
	for _, item := range envelope.Output {
		blocks, err := responseBlocks(item)
		if err != nil {
			return response, err
		}
		for _, b := range blocks {
			if len(response.Content) >= 4096 {
				return response, malformed()
			}
			if b.Type == "tool_use" && response.StopReason != "max_tokens" {
				response.StopReason = "tool_use"
			}
			response.Content = append(response.Content, b)
		}
	}
	usage, err := openAIUsageOf(envelope.Usage)
	if err != nil {
		return response, err
	}
	response.Usage = usage
	return response, finalize(&response)
}

func parseResponses(reader io.Reader, isJSON bool, emit func(core.Delta)) (core.Response, bool, error) {
	if isJSON {
		data, err := boundedJSON(reader)
		if err != nil {
			return core.Response{}, false, err
		}
		response, err := responsesFull(data)
		if err == nil {
			for i, b := range response.Content {
				emit(core.Delta{Type: "block_start", Index: i, Text: b.Type})
				if b.Text != "" {
					emit(core.Delta{Type: "text", Index: i, Text: b.Text})
				}
				if b.Thinking != "" {
					emit(core.Delta{Type: "thinking", Index: i, Text: b.Thinking})
				}
				emit(core.Delta{Type: "block_stop", Index: i})
			}
		}
		return response, len(response.Content) > 0, err
	}
	response := core.Response{}
	indices := map[string]int{}
	items := map[int]string{}
	closedItems := map[int]bool{}
	closedBlocks := map[int]bool{}
	started := false
	ensure := func(key, kind string) int {
		if index, ok := indices[key]; ok {
			return index
		}
		index := len(response.Content)
		indices[key] = index
		response.Content = append(response.Content, core.Block{Type: kind})
		emit(core.Delta{Type: "block_start", Index: index, Text: kind})
		started = true
		return index
	}
	err := consumeSSE(reader, func(eventName, frame string) (bool, error) {
		var event struct {
			Type         string          `json:"type"`
			OutputIndex  *int            `json:"output_index"`
			ContentIndex *int            `json:"content_index"`
			Delta        string          `json:"delta"`
			Item         json.RawMessage `json:"item"`
			Response     json.RawMessage `json:"response"`
		}
		if !utf8.ValidString(frame) || json.Unmarshal([]byte(frame), &event) != nil || event.Type == "" || (eventName != "" && eventName != event.Type) {
			return false, malformed()
		}
		switch event.Type {
		case "response.created", "response.in_progress":
			var meta struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(event.Response, &meta) != nil || meta.ID == "" {
				return false, malformed()
			}
			if response.ID != "" && response.ID != meta.ID {
				return false, malformed()
			}
			response.ID = meta.ID
		case "response.output_item.added":
			if event.OutputIndex == nil || *event.OutputIndex < 0 || *event.OutputIndex >= 4096 {
				return false, malformed()
			}
			var item responseItem
			if json.Unmarshal(event.Item, &item) != nil {
				return false, malformed()
			}
			if _, exists := items[*event.OutputIndex]; exists {
				return false, malformed()
			}
			items[*event.OutputIndex] = item.Type
			key := fmt.Sprintf("item:%d", *event.OutputIndex)
			switch item.Type {
			case "function_call":
				index := ensure(key, "tool_use")
				response.Content[index].ID = item.CallID
				response.Content[index].Name = item.Name
				response.Content[index].Data = item.Arguments
			case "reasoning":
				index := ensure(key, "thinking")
				var source map[string]any
				_ = json.Unmarshal(event.Item, &source)
				response.Content[index].Source = source
			case "message":
				started = true
			default:
				return false, &Error{Code: "UNSUPPORTED_CONTENT"}
			}
		case "response.output_text.delta", "response.refusal.delta", "response.reasoning_summary_text.delta", "response.function_call_arguments.delta":
			if event.OutputIndex == nil || *event.OutputIndex < 0 || *event.OutputIndex >= 4096 {
				return false, malformed()
			}
			key := fmt.Sprintf("item:%d", *event.OutputIndex)
			kind, exists := items[*event.OutputIndex]
			if !exists || closedItems[*event.OutputIndex] {
				return false, malformed()
			}
			if event.Type == "response.output_text.delta" || event.Type == "response.refusal.delta" {
				if kind != "message" || event.ContentIndex == nil || *event.ContentIndex < 0 || *event.ContentIndex >= 4096 {
					return false, malformed()
				}
				key += fmt.Sprintf(":part:%d", *event.ContentIndex)
				if _, exists := indices[key]; !exists && len(indices) >= 4096 {
					return false, malformed()
				}
				index := ensure(key, "text")
				response.Content[index].Text += event.Delta
				emit(core.Delta{Type: "text", Index: index, Text: event.Delta})
			} else if event.Type == "response.reasoning_summary_text.delta" {
				if kind != "reasoning" {
					return false, malformed()
				}
				index := ensure(key, "thinking")
				response.Content[index].Thinking += event.Delta
				emit(core.Delta{Type: "thinking", Index: index, Text: event.Delta})
			} else {
				if kind != "function_call" {
					return false, malformed()
				}
				index, ok := indices[key]
				if !ok || response.Content[index].Type != "tool_use" {
					return false, malformed()
				}
				response.Content[index].Data += event.Delta
				emit(core.Delta{Type: "tool_input", Index: index, Text: event.Delta})
			}
		case "response.output_item.done":
			if event.OutputIndex == nil || *event.OutputIndex < 0 || *event.OutputIndex >= 4096 {
				return false, malformed()
			}
			key := fmt.Sprintf("item:%d", *event.OutputIndex)
			blocks, err := responseBlocks(event.Item)
			if err != nil {
				return false, err
			}
			var meta responseItem
			_ = json.Unmarshal(event.Item, &meta)
			kind, exists := items[*event.OutputIndex]
			if !exists || kind != meta.Type || closedItems[*event.OutputIndex] {
				return false, malformed()
			}
			closedItems[*event.OutputIndex] = true
			for n, b := range blocks {
				blockKey := key
				if b.Type == "text" {
					blockKey += fmt.Sprintf(":part:%d", n)
				}
				if _, exists := indices[blockKey]; !exists && len(indices) >= 4096 {
					return false, malformed()
				}
				index := ensure(blockKey, b.Type)
				previous := response.Content[index]
				if err := emitRemainder(previous, b, index, emit); err != nil {
					return false, err
				}
				response.Content[index] = b
				closedBlocks[index] = true
			}
		case "response.completed", "response.incomplete":
			complete, err := responsesFull(event.Response)
			if err != nil {
				return false, err
			}
			if response.ID != "" && complete.ID != response.ID {
				return false, malformed()
			}
			if event.Type == "response.completed" && complete.StopReason == "max_tokens" || event.Type == "response.incomplete" && complete.StopReason != "max_tokens" {
				return false, malformed()
			}
			if len(response.Content) > len(complete.Content) {
				return false, malformed()
			}
			for i, b := range complete.Content {
				previous := core.Block{Type: b.Type}
				if i < len(response.Content) {
					previous = response.Content[i]
				} else {
					emit(core.Delta{Type: "block_start", Index: i, Text: b.Type})
				}
				if err := emitRemainder(previous, b, i, emit); err != nil {
					return false, err
				}
				if b.Type == "thinking" {
					// The documented replay source is the settled item.done value,
					// never the possibly incomplete item.added encrypted payload.
					complete.Content[i].Source = nil
					if closedBlocks[i] {
						complete.Content[i].Source = previous.Source
					}
				}
			}
			response = complete
			for i := range response.Content {
				emit(core.Delta{Type: "block_stop", Index: i})
			}
			return true, nil
		case "response.failed", "error":
			return false, &Error{Code: "SERVER"}
		}
		return false, nil
	})
	return response, started, err
}

// A settled item may complete content omitted from deltas, but may not revise
// text already shown to the user or change a streamed tool's identity.
func emitRemainder(previous, next core.Block, index int, emit func(core.Delta)) error {
	if previous.Type != next.Type {
		return malformed()
	}
	switch next.Type {
	case "text":
		if !strings.HasPrefix(next.Text, previous.Text) {
			return malformed()
		}
		if suffix := strings.TrimPrefix(next.Text, previous.Text); suffix != "" {
			emit(core.Delta{Type: "text", Index: index, Text: suffix})
		}
	case "thinking":
		if !strings.HasPrefix(next.Thinking, previous.Thinking) {
			return malformed()
		}
		if suffix := strings.TrimPrefix(next.Thinking, previous.Thinking); suffix != "" {
			emit(core.Delta{Type: "thinking", Index: index, Text: suffix})
		}
	case "tool_use":
		if previous.ID != "" && previous.ID != next.ID || previous.Name != "" && previous.Name != next.Name {
			return malformed()
		}
		args := string(next.Input)
		if args == "" {
			args = next.Data
		}
		oldArgs := string(previous.Input)
		if oldArgs == "" {
			oldArgs = previous.Data
		}
		if !strings.HasPrefix(args, oldArgs) {
			return malformed()
		}
		if suffix := strings.TrimPrefix(args, oldArgs); suffix != "" {
			emit(core.Delta{Type: "tool_input", Index: index, Text: suffix})
		}
	}
	return nil
}
