package model

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"

	"dsgo/internal/core"
)

// ProviderOptions configures one of the three explicit wire protocols offered
// by official llm-pi-ai. It does not recreate pi-ai's full provider catalog or
// its provider-native OAuth, AWS signing, Azure or Vertex credential discovery.
type ProviderOptions struct {
	API         string            `json:"api"` // openai-completions, openai-responses, anthropic-messages
	BaseURL     string            `json:"base_url,omitempty"`
	APIKey      string            `json:"api_key,omitempty"`
	APIKeyEnv   string            `json:"api_key_env,omitempty"`
	HTTPClient  *http.Client      `json:"-"`
	Headers     map[string]string `json:"headers,omitempty"`
	MaxRetries  int               `json:"max_retries,omitempty"`
	RetryDelay  time.Duration     `json:"-"`
	IdleTimeout time.Duration     `json:"-"`
	// Anthropic thinking must be explicitly configured for the selected model.
	ThinkingMode   string `json:"thinking_mode,omitempty"` // adaptive or budget; empty supports off only
	ThinkingBudget int    `json:"thinking_budget,omitempty"`
	// Optional model-specific mapping, e.g. max -> xhigh. Off may map to empty.
	ReasoningEfforts         map[string]string `json:"reasoning_efforts,omitempty"`
	MaxTokensField           string            `json:"max_tokens_field,omitempty"`           // Chat: max_tokens or max_completion_tokens
	SystemRole               string            `json:"system_role,omitempty"`                // Chat: system or developer
	RequiresReasoningContent bool              `json:"requires_reasoning_content,omitempty"` // Chat gateway's reasoning_content replay
	OmitStreamingUsage       bool              `json:"omit_streaming_usage,omitempty"`
	OmitMaxOutputTokens      bool              `json:"omit_max_output_tokens,omitempty"` // Responses gateway compatibility
}

type ProviderClient struct {
	options   ProviderOptions
	transport *Client
	headers   http.Header
}

var _ core.Model = (*ProviderClient)(nil)

func NewProvider(options ProviderOptions) (*ProviderClient, error) {
	switch options.API {
	case "openai-completions", "openai-responses":
		if options.BaseURL == "" {
			options.BaseURL = "https://api.openai.com/v1"
		}
	case "anthropic-messages":
		if options.BaseURL == "" {
			options.BaseURL = "https://api.anthropic.com/v1"
		}
	default:
		return nil, &Error{Code: "UNSUPPORTED_PROTOCOL"}
	}
	if options.MaxTokensField == "" {
		options.MaxTokensField = "max_completion_tokens"
	}
	if options.MaxTokensField != "max_tokens" && options.MaxTokensField != "max_completion_tokens" {
		return nil, invalid()
	}
	if options.SystemRole == "" {
		options.SystemRole = "system"
	}
	if options.SystemRole != "system" && options.SystemRole != "developer" {
		return nil, invalid()
	}
	if options.ThinkingMode != "" && options.ThinkingMode != "adaptive" && options.ThinkingMode != "budget" {
		return nil, invalid()
	}
	if options.ThinkingMode == "budget" && options.ThinkingBudget < 1024 {
		return nil, invalid()
	}
	if options.APIKeyEnv != "" {
		for i, r := range options.APIKeyEnv {
			if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
				return nil, &Error{Code: "INVALID_CREDENTIAL_REFERENCE"}
			}
		}
	}
	transport := New(Options{BaseURL: options.BaseURL, APIKey: options.APIKey, HTTPClient: options.HTTPClient, MaxRetries: options.MaxRetries, RetryDelay: options.RetryDelay, IdleTimeout: options.IdleTimeout, DisableFiles: true})
	if options.MaxRetries == 0 {
		transport.retries = 5
	}
	if transport.initErr != nil {
		return nil, transport.initErr
	}
	// Unlike the direct DeepSeek adapter, pi-ai sends inference requests to the
	// configured base unchanged. Only its model-discovery path normalizes /v1.
	transport.root = strings.TrimRight(options.BaseURL, "/")
	headers := make(http.Header)
	for key, value := range options.Headers {
		switch strings.ToLower(key) {
		case "authorization", "x-api-key", "x-dsh-auth-token", "host", "content-length", "transfer-encoding", "connection", "content-type", "user-agent":
			return nil, &Error{Code: "RESERVED_HEADER"}
		}
		if !headerName(key) || strings.IndexFunc(value, func(r rune) bool { return r < 32 && r != '\t' || r == 127 }) >= 0 {
			return nil, invalid()
		}
		headers.Set(key, value)
	}
	mapping := map[string]string{}
	for key, value := range options.ReasoningEfforts {
		if !thinkingLevel(key) || strings.ContainsAny(value, "\r\n\x00") {
			return nil, invalid()
		}
		mapping[key] = value
	}
	options.ReasoningEfforts = mapping
	return &ProviderClient{options: options, transport: transport, headers: headers}, nil
}

func headerName(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r)) {
			return false
		}
	}
	return true
}

func (p *ProviderClient) credential() (string, error) {
	key := p.options.APIKey
	if p.options.APIKeyEnv != "" {
		key = os.Getenv(p.options.APIKeyEnv)
	}
	if key == "" {
		return "", &Error{Code: "MISSING_CREDENTIAL"}
	}
	if strings.IndexFunc(key, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", &Error{Code: "INVALID_CREDENTIAL"}
	}
	return key, nil
}

func thinkingLevel(value string) bool {
	switch value {
	case "off", "minimal", "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
}

func (p *ProviderClient) Stream(ctx context.Context, input core.Request, emit func(core.Delta)) (core.Response, error) {
	if err := ctx.Err(); err != nil {
		return core.Response{}, err
	}
	key, err := p.credential()
	if err != nil {
		return core.Response{}, err
	}
	// This matches the official pi-ai seam, whose common stream options cannot
	// guarantee stop sequences across all its protocols.
	if len(input.StopSequences) > 0 {
		return core.Response{}, &Error{Code: "UNSUPPORTED_OPTION"}
	}
	payload, err := p.payload(input)
	if err != nil {
		return core.Response{}, err
	}
	data, err := json.Marshal(payload)
	if err != nil || len(data) > maxRequestBytes {
		return core.Response{}, &Error{Code: "REQUEST_LIMIT"}
	}
	if emit == nil {
		emit = func(core.Delta) {}
	}
	endpoint := "/chat/completions"
	if p.options.API == "openai-responses" {
		endpoint = "/responses"
	}
	if p.options.API == "anthropic-messages" {
		endpoint = "/messages"
	}
	for attempt := 0; ; attempt++ {
		callCtx, cancel := context.WithCancel(ctx)
		watch := newIdleWatch(cancel, p.transport.idleTimeout)
		request, requestErr := http.NewRequestWithContext(callCtx, http.MethodPost, p.transport.root+endpoint, bytes.NewReader(data))
		if requestErr != nil {
			watch.stop()
			cancel()
			return core.Response{}, invalid()
		}
		request.Header = p.headers.Clone()
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "text/event-stream, application/json")
		request.Header.Set("User-Agent", "dsgo/0.1 (official Harness wire protocol port)")
		if p.options.API == "anthropic-messages" {
			request.Header.Set("x-api-key", key)
			request.Header.Set("anthropic-version", "2023-06-01")
		} else {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		raw, requestErr := p.transport.http.Do(request)
		var response core.Response
		started := false
		if requestErr != nil {
			err = &Error{Code: "TRANSPORT"}
		} else {
			if raw.StatusCode < 200 || raw.StatusCode >= 300 {
				err = p.transport.responseError(raw)
			} else {
				reader := &activityReader{reader: raw.Body, watch: watch}
				isJSON := strings.HasPrefix(strings.ToLower(raw.Header.Get("Content-Type")), "application/json")
				switch p.options.API {
				case "anthropic-messages":
					if isJSON {
						response, started, err = parseJSONResponse(reader, emit)
					} else {
						response, started, err = parseSSE(reader, emit)
					}
				case "openai-completions":
					response, started, err = parseChat(reader, isJSON, emit)
				case "openai-responses":
					response, started, err = parseResponses(reader, isJSON, emit)
				}
			}
			raw.Body.Close()
		}
		timedOut := watch.stop()
		cancel()
		if ctx.Err() != nil {
			return response, ctx.Err()
		}
		if timedOut {
			err = &Error{Code: "TIMEOUT"}
		}
		if err == nil {
			return response, nil
		}
		if started || attempt >= p.transport.retries || !retryable(err) {
			return response, err
		}
		delay := p.transport.retryDelay * time.Duration(1<<min(attempt, 4))
		if failure, ok := err.(*Error); ok && failure.RetryAfter > delay {
			delay = failure.RetryAfter
		}
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return response, ctx.Err()
		case <-timer.C:
		}
	}
}

func (p *ProviderClient) payload(input core.Request) (map[string]any, error) {
	normal := input
	if normal.Provider == "" {
		normal.Provider = p.options.API
	}
	normal.Thinking = "off"
	normal.StopSequences = nil
	if normal.MaxTokens == 0 {
		normal.MaxTokens = 32768
	}
	wire, err := serialize(normal)
	if err != nil {
		return nil, err
	}
	if imageBytes(wire) > maxInlineImageBytes {
		return nil, &Error{Code: "IMAGE_LIMIT"}
	}
	effort := input.Thinking
	if effort == "" {
		effort = "off"
	}
	if !thinkingLevel(effort) {
		return nil, &Error{Code: "UNSUPPORTED_REASONING_EFFORT"}
	}
	if len(p.options.ReasoningEfforts) > 0 {
		mapped, ok := p.options.ReasoningEfforts[effort]
		if !ok {
			return nil, &Error{Code: "UNSUPPORTED_REASONING_EFFORT"}
		}
		effort = mapped
	}
	if p.options.API == "anthropic-messages" {
		// Anthropic requires its own signed thinking blocks. Provider-neutral
		// history remains visible as text, without inventing a valid signature.
		for i := range wire.Messages {
			for j := range wire.Messages[i].Content {
				b := &wire.Messages[i].Content[j]
				if b.Type == "thinking" && b.Signature == "" {
					*b = core.Block{Type: "text", Text: b.Thinking}
				}
			}
		}
		data, _ := json.Marshal(wire)
		var payload map[string]any
		_ = json.Unmarshal(data, &payload)
		delete(payload, "output_config")
		if effort != "" && effort != "off" {
			switch p.options.ThinkingMode {
			case "adaptive":
				payload["thinking"] = map[string]string{"type": "adaptive"}
				payload["output_config"] = map[string]string{"effort": effort}
			case "budget":
				if p.options.ThinkingBudget >= wire.MaxTokens {
					return nil, invalid()
				}
				payload["thinking"] = map[string]any{"type": "enabled", "budget_tokens": p.options.ThinkingBudget}
			default:
				return nil, &Error{Code: "UNSUPPORTED_REASONING_EFFORT"}
			}
		}
		return payload, nil
	}
	payload := map[string]any{"model": input.Model, "stream": true, "store": false}
	if input.Temperature != nil {
		payload["temperature"] = *input.Temperature
	}
	if p.options.API == "openai-completions" {
		messages, err := p.chatMessages(wire)
		if err != nil {
			return nil, err
		}
		payload["messages"] = messages
		payload[p.options.MaxTokensField] = wire.MaxTokens
		if !p.options.OmitStreamingUsage {
			payload["stream_options"] = map[string]bool{"include_usage": true}
		}
		if effort != "" && effort != "off" {
			payload["reasoning_effort"] = effort
		}
		if len(input.Tools) > 0 {
			tools := []any{}
			for _, tool := range input.Tools {
				tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": tool.Name, "description": tool.Description, "parameters": tool.InputSchema}})
			}
			payload["tools"] = tools
		}
	} else {
		items, err := p.responseInput(wire)
		if err != nil {
			return nil, err
		}
		payload["input"] = items
		if wire.System != "" {
			payload["instructions"] = wire.System
		}
		if !p.options.OmitMaxOutputTokens {
			payload["max_output_tokens"] = wire.MaxTokens
		}
		if effort != "" && effort != "off" {
			payload["reasoning"] = map[string]string{"effort": effort, "summary": "auto"}
			payload["include"] = []string{"reasoning.encrypted_content"}
		}
		if len(input.Tools) > 0 {
			tools := []any{}
			for _, tool := range input.Tools {
				tools = append(tools, map[string]any{"type": "function", "name": tool.Name, "description": tool.Description, "parameters": tool.InputSchema, "strict": false})
			}
			payload["tools"] = tools
		}
	}
	return payload, nil
}

func (p *ProviderClient) chatMessages(wire wireRequest) ([]any, error) {
	result := []any{}
	if wire.System != "" {
		result = append(result, map[string]any{"role": p.options.SystemRole, "content": wire.System})
	}
	for _, message := range wire.Messages {
		text := ""
		thinking := ""
		calls := []any{}
		parts := []any{}
		results := []any{}
		images := []any{}
		for _, b := range message.Content {
			switch b.Type {
			case "text":
				text += b.Text
				parts = append(parts, map[string]string{"type": "text", "text": b.Text})
			case "thinking":
				thinking += b.Thinking
			case "redacted_thinking":
				// Opaque Anthropic content has no Chat Completions representation.
			case "tool_use":
				calls = append(calls, map[string]any{"id": b.ID, "type": "function", "function": map[string]string{"name": b.Name, "arguments": string(b.Input)}})
			case "image":
				image, err := imageURL(b)
				if err != nil {
					return nil, err
				}
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": image}})
			case "tool_result":
				blocks, err := toolContent(b.Content)
				if err != nil {
					return nil, err
				}
				content := ""
				for _, part := range blocks {
					if part.Type == "text" {
						content += part.Text
					} else {
						image, err := imageURL(part)
						if err != nil {
							return nil, err
						}
						images = append(images, map[string]any{"type": "image_url", "image_url": map[string]string{"url": image}})
					}
				}
				results = append(results, map[string]any{"role": "tool", "tool_call_id": b.ToolUseID, "content": content})
			default:
				return nil, &Error{Code: "UNSUPPORTED_CONTENT"}
			}
		}
		result = append(result, results...)
		if len(images) > 0 {
			result = append(result, map[string]any{"role": "user", "content": images})
		}
		if message.Role == "assistant" {
			value := map[string]any{"role": "assistant", "content": text}
			if len(calls) > 0 {
				value["tool_calls"] = calls
			}
			if thinking != "" && p.options.RequiresReasoningContent {
				value["reasoning_content"] = thinking
			}
			result = append(result, value)
		} else if len(parts) > 0 {
			role := message.Role
			if role == "system" {
				role = p.options.SystemRole
			}
			result = append(result, map[string]any{"role": role, "content": parts})
		}
	}
	return result, nil
}

func imageURL(b core.Block) (string, error) {
	source := sourceOf(&b)
	if source["type"] != "base64" {
		return "", &Error{Code: "UNSUPPORTED_CONTENT"}
	}
	return "data:" + source["media_type"] + ";base64," + source["data"], nil
}

func (p *ProviderClient) responseInput(wire wireRequest) ([]any, error) {
	result := []any{}
	for _, message := range wire.Messages {
		parts := []any{}
		flush := func() {
			if len(parts) > 0 {
				result = append(result, map[string]any{"role": message.Role, "content": parts})
				parts = []any{}
			}
		}
		for _, b := range message.Content {
			switch b.Type {
			case "text":
				kind := "input_text"
				if message.Role == "assistant" {
					kind = "output_text"
				}
				parts = append(parts, map[string]string{"type": kind, "text": b.Text})
			case "image":
				image, err := imageURL(b)
				if err != nil {
					return nil, err
				}
				parts = append(parts, map[string]string{"type": "input_image", "image_url": image})
			case "thinking":
				flush()
				data, _ := json.Marshal(b.Source)
				var item map[string]any
				if json.Unmarshal(data, &item) == nil && item["type"] == "reasoning" {
					result = append(result, item)
				} else if b.Thinking != "" {
					parts = append(parts, map[string]string{"type": "output_text", "text": b.Thinking})
				}
			case "redacted_thinking":
				// Opaque Anthropic content has no Responses representation.
			case "tool_use":
				flush()
				result = append(result, map[string]string{"type": "function_call", "call_id": b.ID, "name": b.Name, "arguments": string(b.Input)})
			case "tool_result":
				flush()
				blocks, err := toolContent(b.Content)
				if err != nil {
					return nil, err
				}
				text := ""
				var images []any
				for _, part := range blocks {
					if part.Type == "text" {
						text += part.Text
					} else {
						image, err := imageURL(part)
						if err != nil {
							return nil, err
						}
						images = append(images, map[string]string{"type": "input_image", "image_url": image})
					}
				}
				result = append(result, map[string]string{"type": "function_call_output", "call_id": b.ToolUseID, "output": text})
				if len(images) > 0 {
					result = append(result, map[string]any{"role": "user", "content": images})
				}
			default:
				return nil, &Error{Code: "UNSUPPORTED_CONTENT"}
			}
		}
		flush()
	}
	return result, nil
}

func boundedJSON(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxResponseBytes+1))
	if err != nil {
		return nil, closedStream()
	}
	if len(data) > maxResponseBytes {
		return nil, &Error{Code: "RESPONSE_LIMIT"}
	}
	return data, nil
}
