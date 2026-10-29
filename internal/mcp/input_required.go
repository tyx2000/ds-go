package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// The official bridge declares no client interaction capabilities/handlers.
// The only auto-fulfilment it can perform is a requestState-only continuation.
type InputCapabilityError struct {
	Key, Method string
	Payload     *InputRequiredError
}

func (e *InputCapabilityError) Error() string {
	return fmt.Sprintf("Cannot fulfil input request '%s': no handler is registered for '%s' on this client. Declare the corresponding capability and register a handler, or handle input_required results manually.", e.Key, e.Method)
}
func (e *InputCapabilityError) Unwrap() error { return e.Payload }

type InputRoundsExceededError struct {
	Method  string
	Payload *InputRequiredError
}

func (e *InputRoundsExceededError) Error() string {
	return fmt.Sprintf("Multi-round-trip request '%s' still required input after 10 rounds (inputRequired.maxRounds)", e.Method)
}
func (e *InputRoundsExceededError) Unwrap() error { return e.Payload }

func (c *Client) callWithInputDriver(ctx context.Context, method string, params any, out any) error {
	flowCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if c.lifetime != nil {
		stop := context.AfterFunc(c.lifetime, cancel)
		defer stop()
	}
	current := params
	for round := 0; ; round++ {
		id := json.RawMessage(strconv.FormatInt(c.seq.Add(1), 10))
		err := c.callWithID(flowCtx, method, current, out, id, false)
		var required *InputRequiredError
		if !errors.As(err, &required) {
			return err
		}
		if round >= 10 {
			return &InputRoundsExceededError{Method: method, Payload: required}
		}
		if len(required.InputRequests) > 0 {
			keys := make([]string, 0, len(required.InputRequests))
			for key := range required.InputRequests {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			key := keys[0]
			var request struct {
				Method string `json:"method"`
			}
			if json.Unmarshal(required.InputRequests[key], &request) != nil || request.Method == "" {
				return fmt.Errorf("invalid input request %q: each inputRequests entry must be an embedded request object with a method", key)
			}
			switch request.Method {
			case "elicitation/create", "sampling/createMessage", "roots/list":
				return &InputCapabilityError{Key: key, Method: request.Method, Payload: required}
			}
			return fmt.Errorf("invalid input request %q: %q is not an embedded request the 2026-07-28 revision defines", key, request.Method)
		}
		if err := waitSSE(flowCtx, 250*time.Millisecond); err != nil {
			return err
		}
		original := map[string]json.RawMessage{}
		if params != nil {
			data, err := json.Marshal(params)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(data, &original); err != nil || original == nil {
				return errors.New("MCP continuation params must be an object")
			}
		}
		if required.HasRequestState {
			original["requestState"] = required.RequestStateRaw
		}
		current = original
	}
}
