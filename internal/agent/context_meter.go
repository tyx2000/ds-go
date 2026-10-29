package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"unicode/utf16"

	"dsgo/internal/core"
)

// ContextImagePrice is the route's actual image projection: visual tokens plus
// the model-visible handle/placeholder text. No pricing means fixed structure.
type ContextImagePrice struct {
	VisualTokens int
	Text         string
}

// ContextRoute captures model capacity and request attachment projections.
// Pricing callbacks must use the same route facts and text as serialization.
type ContextRoute struct {
	ContextWindow    int
	DefaultMaxTokens int
	ImagePricing     func([]core.Block) ([]ContextImagePrice, error)
	FileText         func(core.Block) (string, error)
}

type ContextPriceNode struct {
	Origin          core.ContextOrigin `json:"origin"`
	Tokens          int                `json:"tokens"`
	HeuristicTokens int                `json:"heuristic_tokens"`
}

type ContextMeasurement struct {
	BaselineKind       string             `json:"baseline_kind"`
	BaselineTokens     int                `json:"baseline_tokens"`
	SurfaceDeltaTokens int                `json:"surface_delta_tokens"`
	TotalTokens        int                `json:"total_tokens"`
	SurfaceTokens      int                `json:"surface_tokens"`
	Nodes              []ContextPriceNode `json:"nodes"`
}

type contextTool struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Parameters   any    `json:"parameters"`
	DeferLoading bool   `json:"deferLoading,omitempty"`
}

// The system prompt is a surface contribution, not a priced envelope field.
type contextHeader struct {
	Provider           string        `json:"provider"`
	Model              string        `json:"model"`
	Thinking           string        `json:"thinking,omitempty"`
	MaxTokens          int           `json:"maxTokens"`
	Temperature        *float64      `json:"temperature,omitempty"`
	StopSequences      []string      `json:"stopSequences,omitempty"`
	SystemPromptUpdate string        `json:"systemPromptUpdate,omitempty"`
	ToolUpdate         string        `json:"toolUpdate,omitempty"`
	Tools              []contextTool `json:"tools,omitempty"`
}

type contextRequestRecord struct {
	Version int            `json:"version"`
	Header  contextHeader  `json:"header"`
	System  string         `json:"system"`
	Extras  []core.Message `json:"extras,omitempty"`
}

func contextHeaderFor(req core.Request) (contextHeader, error) {
	header := contextHeader{Provider: req.Provider, Model: req.Model, Thinking: req.Thinking, MaxTokens: req.MaxTokens, Temperature: req.Temperature, StopSequences: req.StopSequences, SystemPromptUpdate: req.SystemPromptUpdate, ToolUpdate: req.ToolUpdate}
	for _, tool := range req.Tools {
		var parameters any
		if err := json.Unmarshal(tool.InputSchema, &parameters); err != nil {
			return header, fmt.Errorf("context tool schema %s: %w", tool.Name, err)
		}
		header.Tools = append(header.Tools, contextTool{tool.Name, tool.Description, parameters, tool.DeferLoading})
	}
	return header, nil
}

func contextUnits(text string) int      { return len(utf16.Encode([]rune(text))) }
func contextTextTokens(text string) int { return (contextUnits(text)+3)/4 + 4 }

// contextJSON uses JavaScript-compatible HTML and line-separator spelling for
// structural prices. Validated JSON values are the only inputs to this helper.
func contextJSON(value any) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	raw := bytes.TrimSuffix(out.Bytes(), []byte{'\n'})
	var normalized bytes.Buffer
	for index := 0; index < len(raw); {
		if raw[index] == '\\' && index+1 < len(raw) {
			if bytes.HasPrefix(raw[index:], []byte(`\u2028`)) {
				normalized.WriteRune('\u2028')
				index += 6
				continue
			}
			if bytes.HasPrefix(raw[index:], []byte(`\u2029`)) {
				normalized.WriteRune('\u2029')
				index += 6
				continue
			}
			normalized.Write(raw[index : index+2])
			index += 2
			continue
		}
		normalized.WriteByte(raw[index])
		index++
	}
	return normalized.Bytes(), nil
}

func contextStructuralTokens(value any) (int, error) {
	raw, err := contextJSON(value)
	return (contextUnits(string(raw))+3)/4 + 4, err
}

func contextResultBlocks(value any) ([]core.Block, error) {
	if value == nil {
		return nil, nil
	}
	if text, ok := value.(string); ok {
		return []core.Block{{Type: "text", Text: text}}, nil
	}
	if blocks, ok := value.([]core.Block); ok {
		return blocks, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var blocks []core.Block
	if json.Unmarshal(raw, &blocks) == nil {
		return blocks, nil
	}
	// A native tool may return a structured JSON value rather than rich
	// blocks. It reaches the model as text, not as canonical PTC transport.
	return []core.Block{{Type: "text", Text: string(raw)}}, nil
}

// Logical tool-role messages are stored as one user-role tool-results batch in
// dsgo. Price each result with its own role overhead, as in the official meter.
func contextLogicalMessages(message core.Message) ([]core.Message, error) {
	var out []core.Message
	ordinary := message
	ordinary.Content = nil
	for _, block := range message.Content {
		if block.Type != "tool_result" {
			ordinary.Content = append(ordinary.Content, block)
			continue
		}
		blocks, err := contextResultBlocks(block.Content)
		if err != nil {
			return nil, err
		}
		out = append(out, core.Message{Role: "tool", Content: blocks})
	}
	if len(ordinary.Content) > 0 || len(out) == 0 {
		out = append(out, ordinary)
	}
	return out, nil
}

func contextBlockTokens(block core.Block, route ContextRoute, images []ContextImagePrice, imageCursor *int) (int, error) {
	switch block.Type {
	case "text":
		return contextTextTokens(block.Text), nil
	case "reasoning", "thinking":
		text := block.Text
		if text == "" {
			text = block.Thinking
		}
		return contextTextTokens(text), nil
	case "tool_use", "tool-call":
		return (contextUnits(block.Name)+3)/4 + (contextUnits(string(block.Input))+3)/4 + 4, nil
	case "image":
		if route.ImagePricing != nil {
			if *imageCursor >= len(images) {
				return 0, errors.New("context image pricing lost occurrence alignment")
			}
			price := images[*imageCursor]
			*imageCursor++
			if price.VisualTokens < 0 {
				return 0, errors.New("context image price must be nonnegative")
			}
			return price.VisualTokens + contextTextTokens(price.Text), nil
		}
	case "file":
		if route.FileText != nil {
			text, err := route.FileText(block)
			return contextTextTokens(text), err
		}
	case "tool_addition", "tool-addition":
		return contextStructuralTokens(map[string]any{"type": "tool-addition", "toolName": block.ToolName})
	}
	return contextStructuralTokens(block)
}

func priceContextMessages(messages []core.Message, route ContextRoute) ([]int, error) {
	logical := make([][]core.Message, len(messages))
	var images []core.Block
	for index, message := range messages {
		parts, err := contextLogicalMessages(message)
		if err != nil {
			return nil, err
		}
		logical[index] = parts
		for _, part := range parts {
			for _, block := range part.Content {
				if block.Type == "image" {
					images = append(images, block)
				}
			}
		}
	}
	var prices []ContextImagePrice
	if route.ImagePricing != nil && len(images) > 0 {
		var err error
		prices, err = route.ImagePricing(images)
		if err != nil {
			return nil, err
		}
		if len(prices) != len(images) {
			return nil, fmt.Errorf("context image pricing answered %d prices for %d occurrences", len(prices), len(images))
		}
	}
	tokens := make([]int, len(messages))
	imageCursor := 0
	for index, parts := range logical {
		for _, part := range parts {
			if part.Role == "system" {
				if len(part.Content) == 0 {
					continue
				}
				characters := 0
				for _, block := range part.Content {
					if block.Type == "text" {
						characters += contextUnits(block.Text)
					} else {
						raw, err := contextJSON(block)
						if err != nil {
							return nil, err
						}
						characters += contextUnits(string(raw))
					}
				}
				tokens[index] += (characters+3)/4 + 4
				continue
			}
			tokens[index] += 4
			for _, block := range part.Content {
				price, err := contextBlockTokens(block, route, prices, &imageCursor)
				if err != nil {
					return nil, err
				}
				tokens[index] += price
			}
		}
	}
	return tokens, nil
}

// EstimateContextMessage is the fixed-density, route-neutral heuristic. Text
// uses UTF-16 length / 4 rounded up, with four tokens per block and role.
func EstimateContextMessage(message core.Message) (int, error) {
	prices, err := priceContextMessages([]core.Message{message}, ContextRoute{})
	if err != nil {
		return 0, err
	}
	return prices[0], nil
}

func contextToolsTokens(header contextHeader) (int, error) {
	if len(header.Tools) == 0 {
		return 0, nil
	}
	return contextStructuralTokens(header.Tools)
}

func (e *Engine) contextRoute(ctx context.Context, req core.Request) (ContextRoute, error) {
	opts := e.options()
	if opts.ContextFor != nil {
		return opts.ContextFor(ctx, req)
	}
	return ContextRoute{ContextWindow: opts.ContextTokens}, nil
}

func (e *Engine) contextSurface(id string) ([]core.ContextNode, []core.Event, error) {
	if store, ok := e.Store.(interface {
		Events(string) ([]core.Event, error)
	}); ok {
		events, err := store.Events(id)
		if err != nil {
			return nil, nil, err
		}
		nodes, err := core.ContextSurface(events)
		return nodes, events, err
	}
	messages, err := e.Store.Messages(id)
	if err != nil {
		return nil, nil, err
	}
	nodes := make([]core.ContextNode, len(messages))
	for index, message := range messages {
		nodes[index] = core.ContextNode{Origin: core.ContextOrigin{Seq: -int64(index + 1)}, Message: message}
	}
	return nodes, nil, nil
}

func contextRequestParts(req core.Request, nodes []core.ContextNode) (contextRequestRecord, error) {
	header, err := contextHeaderFor(req)
	if err != nil {
		return contextRequestRecord{}, err
	}
	if len(req.Messages) < len(nodes) {
		return contextRequestRecord{}, errors.New("context request omits current durable messages")
	}
	for index, node := range nodes {
		if !bytes.Equal(core.JSON(node.Message), core.JSON(req.Messages[index])) {
			return contextRequestRecord{}, errors.New("context request differs from durable surface")
		}
	}
	return contextRequestRecord{Version: 1, Header: header, System: req.System, Extras: req.Messages[len(nodes):]}, nil
}

func pricedContextSurface(nodes []core.ContextNode, request contextRequestRecord, route ContextRoute) ([]ContextPriceNode, int, error) {
	messages := []core.Message{}
	origins := []core.ContextOrigin{}
	if request.System != "" {
		messages = append(messages, core.TextMessage("system", request.System))
		origins = append(origins, core.ContextOrigin{})
	}
	for _, node := range nodes {
		messages = append(messages, node.Message)
		origins = append(origins, node.Origin)
	}
	for index, message := range request.Extras {
		messages = append(messages, message)
		origins = append(origins, core.ContextOrigin{Part: index + 1})
	}
	prices, err := priceContextMessages(messages, route)
	if err != nil {
		return nil, 0, err
	}
	heuristic, err := priceContextMessages(messages, ContextRoute{})
	if err != nil {
		return nil, 0, err
	}
	priced := make([]ContextPriceNode, len(messages))
	total := 0
	for index := range messages {
		priced[index] = ContextPriceNode{origins[index], prices[index], heuristic[index]}
		total += prices[index]
	}
	return priced, total, nil
}

// MeasureContext replays the latest successful request anchor. Usage is reused
// only for an equal canonical envelope and when no lower than that anchor's
// complete route price; otherwise the fixed heuristic remains authoritative.
func (e *Engine) MeasureContext(ctx context.Context, id string, req core.Request) (ContextMeasurement, error) {
	nodes, events, err := e.contextSurface(id)
	if err != nil {
		return ContextMeasurement{}, err
	}
	request, err := contextRequestParts(req, nodes)
	if err != nil {
		return ContextMeasurement{}, err
	}
	route, err := e.contextRoute(ctx, req)
	if err != nil {
		return ContextMeasurement{}, err
	}
	priced, total, err := pricedContextSurface(nodes, request, route)
	if err != nil {
		return ContextMeasurement{}, err
	}
	tools, err := contextToolsTokens(request.Header)
	if err != nil {
		return ContextMeasurement{}, err
	}
	measurement := ContextMeasurement{BaselineKind: "estimated", BaselineTokens: total + tools, TotalTokens: total + tools, SurfaceTokens: total, Nodes: priced}
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type != "assistant/message" {
			continue
		}
		var assistant core.Message
		if err := json.Unmarshal(events[index].Data, &assistant); err != nil {
			return measurement, err
		}
		var prior contextRequestRecord
		found := false
		for before := index - 1; before >= 0; before-- {
			if events[before].Type == "context/request" && events[before].TurnID == events[index].TurnID {
				if err := json.Unmarshal(events[before].Data, &prior); err != nil || prior.Version != 1 {
					return measurement, errors.New("invalid context request anchor")
				}
				found = true
				break
			}
			if events[before].Type == "assistant/message" || events[before].Type == "session/forked" {
				break
			}
		}
		if !found || !reflect.DeepEqual(prior.Header, request.Header) {
			return measurement, nil
		}
		anchorNodes, err := core.ContextSurface(events[:index])
		if err != nil {
			return measurement, err
		}
		_, anchorSurface, err := pricedContextSurface(anchorNodes, prior, route)
		if err != nil {
			return measurement, err
		}
		assistantTokens, err := EstimateContextMessage(assistant)
		if err != nil {
			return measurement, err
		}
		anchorSurface += assistantTokens
		baseline := tools + anchorSurface
		for after := index + 1; after < len(events); after++ {
			if events[after].Type == "assistant/message" || events[after].Type == "context/request" || events[after].TurnID != events[index].TurnID {
				break
			}
			if events[after].Type == "step/usage" {
				var usage core.Usage
				if err := json.Unmarshal(events[after].Data, &usage); err != nil {
					return measurement, err
				}
				used := usage.InputTokens + usage.CacheReadTokens + usage.CacheWriteTokens + usage.OutputTokens
				if usage.InputTokens < 0 || usage.CacheReadTokens < 0 || usage.CacheWriteTokens < 0 || usage.OutputTokens < 0 {
					return measurement, errors.New("invalid context usage anchor")
				}
				if used >= baseline {
					baseline = used
					measurement.BaselineKind = "usage"
				}
				break
			}
		}
		measurement.BaselineTokens = baseline
		measurement.SurfaceDeltaTokens = total - anchorSurface
		measurement.TotalTokens = max(0, baseline+measurement.SurfaceDeltaTokens)
		return measurement, nil
	}
	return measurement, nil
}
