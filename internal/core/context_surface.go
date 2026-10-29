package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ContextOrigin identifies one projected message within a durable event. Part
// is zero for ordinary messages; an input batch has one part per adjacent input.
type ContextOrigin struct {
	Seq  int64 `json:"seq"`
	Part int   `json:"part"`
}

// ContextNode is one current model-visible message. A nonzero InputGroup keeps
// a direct input adjacent to all of its captured contexts during replacement.
type ContextNode struct {
	Origin     ContextOrigin `json:"origin"`
	InputGroup int64         `json:"input_group,omitempty"`
	Message    Message       `json:"message"`
}

// ContextReplacement is dsgo's native durable surface-replacement event. Start
// is inclusive and End exclusive. Expected names the entire preceding surface;
// even messages outside the replaced range participate in its CAS fence.
type ContextReplacement struct {
	Version  int             `json:"version"`
	Expected []ContextOrigin `json:"expected"`
	Start    int             `json:"start"`
	End      int             `json:"end"`
	Messages []Message       `json:"messages"`
}

func ContextOrigins(nodes []ContextNode) []ContextOrigin {
	origins := make([]ContextOrigin, len(nodes))
	for i := range nodes {
		origins[i] = nodes[i].Origin
	}
	return origins
}

func ContextMessages(nodes []ContextNode) []Message {
	messages := make([]Message, len(nodes))
	for i := range nodes {
		messages[i] = nodes[i].Message
	}
	return messages
}

// ValidateContextReplacement checks the exact surface before an append. It is
// also applied on replay, so a stale or malformed durable replace fails closed.
func ValidateContextReplacement(nodes []ContextNode, replacement ContextReplacement) error {
	if replacement.Version != 1 || replacement.Expected == nil || len(replacement.Expected) != len(nodes) {
		return errors.New("invalid context replacement version or expected surface")
	}
	for i, origin := range replacement.Expected {
		if origin != nodes[i].Origin {
			return errors.New("context surface changed before replacement")
		}
	}
	if replacement.Start < 0 || replacement.End <= replacement.Start || replacement.End > len(nodes) || len(replacement.Messages) == 0 {
		return errors.New("invalid context replacement range or messages")
	}
	for _, cut := range []int{replacement.Start, replacement.End} {
		if cut > 0 && cut < len(nodes) && nodes[cut].InputGroup != 0 && nodes[cut].InputGroup == nodes[cut-1].InputGroup {
			return errors.New("context replacement splits an input batch")
		}
	}
	for _, message := range replacement.Messages {
		if err := validContextMessage(message); err != nil {
			return err
		}
	}
	return nil
}

// ContextSurface projects both native input generations, historical full
// compactions, and native context/replace CAS events through one interpreter.
// It never edits canonical input/tool records or treats a generated snapshot
// as a new direct-input boundary.
func ContextSurface(events []Event) ([]ContextNode, error) {
	nodes := []ContextNode{}
	for _, event := range events {
		if input, recognized, err := UserInputMessages(event); recognized {
			if err != nil {
				return nil, fmt.Errorf("decode %s event %d: %w", event.Type, event.Seq, err)
			}
			group := int64(0)
			if event.Type == "user/input" || directContextInput(input[0]) {
				group = event.Seq
			}
			for part, message := range input {
				nodes = append(nodes, ContextNode{ContextOrigin{event.Seq, part}, group, message})
			}
			continue
		}
		switch event.Type {
		case "assistant/message", "tool/result", "system/message", "developer/message":
			var message Message
			if err := json.Unmarshal(event.Data, &message); err != nil {
				return nil, fmt.Errorf("decode %s event %d: %w", event.Type, event.Seq, err)
			}
			if err := validContextMessage(message); err != nil {
				return nil, fmt.Errorf("invalid message in event %d: %w", event.Seq, err)
			}
			nodes = append(nodes, ContextNode{Origin: ContextOrigin{Seq: event.Seq}, Message: message})
		case "context/compacted":
			var payload struct {
				Messages []Message `json:"messages"`
				Start    *int      `json:"retained_input_start"`
			}
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				return nil, fmt.Errorf("decode context event %d: %w", event.Seq, err)
			}
			if payload.Start != nil && (*payload.Start < 0 || *payload.Start >= len(payload.Messages)) {
				return nil, errors.New("invalid retained input boundary")
			}
			nodes = make([]ContextNode, 0, len(payload.Messages))
			group := int64(0)
			for part, message := range payload.Messages {
				if err := validContextMessage(message); err != nil {
					return nil, fmt.Errorf("invalid compacted message in event %d: %w", event.Seq, err)
				}
				if directContextInput(message) {
					group = -int64(part + 1)
				}
				messageGroup := int64(0)
				if directContextInput(message) || capturedContextInput(message) {
					messageGroup = group
				}
				if payload.Start != nil && part >= *payload.Start {
					messageGroup = -int64(*payload.Start + 1)
				}
				nodes = append(nodes, ContextNode{ContextOrigin{event.Seq, part}, messageGroup, message})
			}
		case "context/replace":
			replacement, err := decodeContextReplacement(event.Data)
			if err != nil {
				return nil, fmt.Errorf("decode context replacement event %d: %w", event.Seq, err)
			}
			if err := ValidateContextReplacement(nodes, replacement); err != nil {
				return nil, fmt.Errorf("context replacement event %d: %w", event.Seq, err)
			}
			next := make([]ContextNode, 0, len(nodes)-(replacement.End-replacement.Start)+len(replacement.Messages))
			next = append(next, nodes[:replacement.Start]...)
			for part, message := range replacement.Messages {
				next = append(next, ContextNode{Origin: ContextOrigin{event.Seq, part}, Message: message})
			}
			nodes = append(next, nodes[replacement.End:]...)
		}
	}
	return nodes, nil
}

func decodeContextReplacement(raw json.RawMessage) (ContextReplacement, error) {
	var replacement ContextReplacement
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return replacement, errors.New("context replacement must be an object")
	}
	for _, key := range []string{"version", "expected", "start", "end", "messages"} {
		value, ok := fields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return replacement, fmt.Errorf("context replacement requires %s", key)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&replacement); err != nil {
		return replacement, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return replacement, errors.New("trailing context replacement value")
	}
	return replacement, nil
}

func validContextMessage(message Message) error {
	if len(message.Content) == 0 {
		return errors.New("context message requires content")
	}
	switch message.Role {
	case "user", "assistant", "system", "developer":
		return nil
	default:
		return errors.New("invalid context message role")
	}
}

func contextSourceKind(message Message) string {
	var source struct{ Kind string }
	_ = json.Unmarshal(JSON(message.Source), &source)
	return source.Kind
}

func capturedContextInput(message Message) bool {
	kind := contextSourceKind(message)
	return kind == "session-reference" || kind == "file-reference"
}

func directContextInput(message Message) bool {
	if message.Role != "user" {
		return false
	}
	for _, block := range message.Content {
		if block.Type == "tool_result" {
			return false
		}
	}
	switch contextSourceKind(message) {
	case "plan-mode", "time-context", "agent-instructions", "compact-checkpoint", "session-reference", "file-reference":
		return false
	default:
		return true
	}
}
