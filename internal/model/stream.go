package model

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"unicode/utf8"

	"dsgo/internal/core"
)

type streamEvent struct {
	Type         string          `json:"type"`
	Index        *int            `json:"index"`
	Message      json.RawMessage `json:"message"`
	ContentBlock json.RawMessage `json:"content_block"`
	Delta        json.RawMessage `json:"delta"`
	Usage        json.RawMessage `json:"usage"`
	Error        struct {
		Type string `json:"type"`
	} `json:"error"`
}
type streamState struct {
	response         core.Response
	started, settled bool
	indices          map[int]int
	closed           map[int]bool
	arguments        map[int]string
	argumentStarted  map[int]bool
}

func malformed() error    { return &Error{Code: "MALFORMED_RESPONSE"} }
func closedStream() error { return &Error{Code: "STREAM_CLOSED"} }

func parseSSE(reader io.Reader, emit func(core.Delta)) (core.Response, bool, error) {
	state := streamState{indices: map[int]int{}, closed: map[int]bool{}, arguments: map[int]string{}, argumentStarted: map[int]bool{}}
	err := consumeSSE(reader, func(eventName, frame string) (bool, error) {
		var event streamEvent
		if !utf8.ValidString(frame) || json.Unmarshal([]byte(frame), &event) != nil || event.Type == "" || (eventName != "" && eventName != event.Type) {
			return false, malformed()
		}
		return state.event(event, emit)
	})
	return state.response, len(state.response.Content) > 0, err
}

// consumeSSE returns only complete frames and requires an explicit protocol
// terminal event from its consumer. Socket EOF alone never means success.
func consumeSSE(reader io.Reader, consume func(string, string) (bool, error)) error {
	count := &countReader{reader: io.LimitReader(reader, maxResponseBytes+1)}
	scanner := bufio.NewScanner(count)
	scanner.Buffer(make([]byte, 4096), maxEventBytes+1)
	scanner.Split(sseLine)
	eventName := ""
	var data strings.Builder
	first := true
	for scanner.Scan() {
		line := scanner.Text()
		if first {
			line = strings.TrimPrefix(line, "\ufeff")
			first = false
		}
		if count.n > maxResponseBytes {
			return &Error{Code: "RESPONSE_LIMIT"}
		}
		if line == "" {
			if data.Len() > 0 {
				done, err := consume(eventName, strings.TrimSuffix(data.String(), "\n"))
				if done || err != nil {
					return err
				}
			}
			data.Reset()
			eventName = ""
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			value = ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			eventName = value
		case "data":
			if data.Len()+len(value)+1 > maxEventBytes {
				return &Error{Code: "RESPONSE_LIMIT"}
			}
			data.WriteString(value)
			data.WriteByte('\n')
		}
	}
	if count.n > maxResponseBytes {
		return &Error{Code: "RESPONSE_LIMIT"}
	}
	return closedStream()
}

func (s *streamState) event(e streamEvent, emit func(core.Delta)) (bool, error) {
	if e.Type == "error" {
		code := "SERVER"
		switch e.Error.Type {
		case "authentication_error", "permission_error":
			code = "AUTH"
		case "rate_limit_error":
			code = "RATE_LIMIT"
		case "invalid_request_error":
			code = "INVALID_REQUEST"
		}
		return false, &Error{Code: code}
	}
	if e.Type == "message_start" {
		if s.started {
			return false, malformed()
		}
		var message struct {
			ID    string          `json:"id"`
			Type  string          `json:"type"`
			Usage json.RawMessage `json:"usage"`
		}
		if json.Unmarshal(e.Message, &message) != nil || message.ID == "" {
			return false, malformed()
		}
		if err := updateUsage(&s.response.Usage, message.Usage); err != nil {
			return false, err
		}
		s.started = true
		s.response.ID = message.ID
		return false, nil
	}
	switch e.Type {
	case "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop":
		if !s.started {
			return false, malformed()
		}
	default:
		return false, nil
	}
	if strings.HasPrefix(e.Type, "content_block_") {
		if e.Index == nil || *e.Index < 0 {
			return false, malformed()
		}
		wire := *e.Index
		if e.Type == "content_block_start" {
			if _, exists := s.indices[wire]; exists || s.settled || len(s.indices) >= 4096 {
				return false, malformed()
			}
			block, err := decodeBlock(e.ContentBlock)
			if err != nil {
				return false, err
			}
			index := len(s.response.Content)
			s.indices[wire] = index
			s.response.Content = append(s.response.Content, block)
			emit(core.Delta{Type: "block_start", Index: index, Text: block.Type})
			if block.Type == "text" && block.Text != "" {
				emit(core.Delta{Type: "text", Index: index, Text: block.Text})
			}
			if block.Type == "thinking" && block.Thinking != "" {
				emit(core.Delta{Type: "thinking", Index: index, Text: block.Thinking})
			}
			return false, nil
		}
		index, exists := s.indices[wire]
		if !exists || s.closed[wire] || s.settled {
			return false, malformed()
		}
		block := &s.response.Content[index]
		if e.Type == "content_block_stop" {
			s.closed[wire] = true
			if s.argumentStarted[wire] {
				if jsonObject([]byte(s.arguments[wire])) {
					block.Input = json.RawMessage(s.arguments[wire])
					block.Data = ""
				} else {
					block.Input = nil
					block.Data = s.arguments[wire]
				}
			}
			emit(core.Delta{Type: "block_stop", Index: index})
			return false, nil
		}
		var delta struct {
			Type        string  `json:"type"`
			Text        *string `json:"text"`
			Thinking    *string `json:"thinking"`
			Signature   *string `json:"signature"`
			PartialJSON *string `json:"partial_json"`
		}
		if json.Unmarshal(e.Delta, &delta) != nil {
			return false, malformed()
		}
		switch {
		case delta.Type == "text_delta" && block.Type == "text" && delta.Text != nil:
			block.Text += *delta.Text
			emit(core.Delta{Type: "text", Index: index, Text: *delta.Text})
		case delta.Type == "thinking_delta" && block.Type == "thinking" && delta.Thinking != nil:
			block.Thinking += *delta.Thinking
			emit(core.Delta{Type: "thinking", Index: index, Text: *delta.Thinking})
		case delta.Type == "signature_delta" && block.Type == "thinking" && delta.Signature != nil:
			block.Signature += *delta.Signature
			emit(core.Delta{Type: "signature", Index: index, Text: *delta.Signature})
		case delta.Type == "input_json_delta" && block.Type == "tool_use" && delta.PartialJSON != nil:
			s.argumentStarted[wire] = true
			s.arguments[wire] += *delta.PartialJSON
			// Keep incomplete JSON in a string so partial responses can always be
			// persisted as JSON even when cancellation splits an argument token.
			block.Data = s.arguments[wire]
			emit(core.Delta{Type: "tool_input", Index: index, Text: *delta.PartialJSON})
		default:
			return false, malformed()
		}
		return false, nil
	}
	if e.Type == "message_delta" {
		var delta struct {
			StopReason *string `json:"stop_reason"`
		}
		if json.Unmarshal(e.Delta, &delta) != nil {
			return false, malformed()
		}
		if delta.StopReason != nil {
			if !validStop(*delta.StopReason) || s.settled {
				return false, malformed()
			}
			s.response.StopReason = *delta.StopReason
			s.settled = true
		}
		return false, updateUsage(&s.response.Usage, e.Usage)
	}
	if !s.settled || len(s.closed) != len(s.indices) {
		return false, malformed()
	}
	if err := finalize(&s.response); err != nil {
		return false, err
	}
	return true, nil
}

func decodeBlock(data json.RawMessage) (core.Block, error) {
	var b core.Block
	if json.Unmarshal(data, &b) != nil {
		return b, malformed()
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(data, &fields)
	requiredString := func(name string) bool {
		var text string
		value, ok := fields[name]
		return ok && string(value) != "null" && json.Unmarshal(value, &text) == nil
	}
	switch b.Type {
	case "text":
		if !requiredString("text") {
			return b, malformed()
		}
		return core.Block{Type: "text", Text: b.Text}, nil
	case "thinking":
		if !requiredString("thinking") {
			return b, malformed()
		}
		if _, ok := fields["signature"]; ok && !requiredString("signature") {
			return b, malformed()
		}
		return core.Block{Type: "thinking", Thinking: b.Thinking, Signature: b.Signature}, nil
	case "redacted_thinking":
		if !requiredString("data") || b.Data == "" {
			return b, malformed()
		}
		return core.Block{Type: "redacted_thinking", Data: b.Data}, nil
	case "tool_use":
		if b.ID == "" || b.Name == "" || !jsonObject(b.Input) {
			return b, malformed()
		}
		return core.Block{Type: "tool_use", ID: b.ID, Name: b.Name, Input: b.Input}, nil
	default:
		return b, &Error{Code: "UNSUPPORTED_CONTENT"}
	}
}

func updateUsage(usage *core.Usage, data json.RawMessage) error {
	if len(data) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return malformed()
	}
	for name, target := range map[string]*int{"input_tokens": &usage.InputTokens, "output_tokens": &usage.OutputTokens, "cache_read_input_tokens": &usage.CacheReadTokens, "cache_creation_input_tokens": &usage.CacheWriteTokens} {
		if raw, ok := fields[name]; ok {
			var value int
			if string(raw) == "null" || json.Unmarshal(raw, &value) != nil || value < 0 {
				return malformed()
			}
			*target = value
		}
	}
	return nil
}
func validStop(reason string) bool {
	switch reason {
	case "end_turn", "stop_sequence", "tool_use", "max_tokens", "pause_turn", "refusal", "model_context_window_exceeded":
		return true
	}
	return false
}
func finalize(response *core.Response) error {
	if len(response.Content) == 0 && (response.StopReason == "end_turn" || response.StopReason == "stop_sequence") {
		return &Error{Code: "EMPTY_RESPONSE"}
	}
	kept := response.Content[:0]
	ids := map[string]bool{}
	for _, b := range response.Content {
		if b.Type == "tool_use" {
			if !jsonObject(b.Input) {
				if response.StopReason == "max_tokens" || response.StopReason == "model_context_window_exceeded" {
					continue
				}
				return malformed()
			}
			if ids[b.ID] {
				return malformed()
			}
			ids[b.ID] = true
		}
		kept = append(kept, b)
	}
	response.Content = kept
	return nil
}

func parseJSONResponse(reader io.Reader, emit func(core.Delta)) (core.Response, bool, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxResponseBytes+1))
	if err != nil {
		return core.Response{}, false, closedStream()
	}
	if len(data) > maxResponseBytes {
		return core.Response{}, false, &Error{Code: "RESPONSE_LIMIT"}
	}
	var raw struct {
		ID         string            `json:"id"`
		Type       string            `json:"type"`
		Content    []json.RawMessage `json:"content"`
		StopReason string            `json:"stop_reason"`
		Usage      json.RawMessage   `json:"usage"`
	}
	if !utf8.Valid(data) || json.Unmarshal(data, &raw) != nil || raw.ID == "" || raw.Type != "message" || !validStop(raw.StopReason) {
		return core.Response{}, false, malformed()
	}
	response := core.Response{ID: raw.ID, StopReason: raw.StopReason}
	for _, content := range raw.Content {
		b, err := decodeBlock(content)
		if err != nil {
			return response, len(response.Content) > 0, err
		}
		response.Content = append(response.Content, b)
	}
	if err := updateUsage(&response.Usage, raw.Usage); err != nil {
		return response, len(response.Content) > 0, err
	}
	if err := finalize(&response); err != nil {
		return response, len(response.Content) > 0, err
	}
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
	return response, len(response.Content) > 0, nil
}

type countReader struct {
	reader io.Reader
	n      int
}

func (r *countReader) Read(p []byte) (int, error) { n, e := r.reader.Read(p); r.n += n; return n, e }

func sseLine(data []byte, atEOF bool) (int, []byte, error) {
	for i, b := range data {
		if b == '\n' {
			return i + 1, data[:i], nil
		}
		if b == '\r' {
			if i+1 == len(data) && !atEOF {
				return 0, nil, nil
			}
			advance := i + 1
			if advance < len(data) && data[advance] == '\n' {
				advance++
			}
			return advance, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), bytes.TrimSuffix(data, []byte{'\r'}), nil
	}
	return 0, nil, nil
}
