package core

import (
	"encoding/json"
	"errors"
)

// InputBatch commits direct input and its captured context in one durable
// record. Projection expands it to adjacent messages, never recapturing data.
type InputBatch struct {
	Version  int       `json:"version"`
	Message  Message   `json:"message"`
	Contexts []Message `json:"contexts"`
}

// UserInputMessages decodes both input generations. A false recognized result
// means the event is unrelated to user admission.
func UserInputMessages(event Event) (messages []Message, recognized bool, err error) {
	switch event.Type {
	case "user/message":
		var message Message
		if err = json.Unmarshal(event.Data, &message); err != nil {
			return nil, true, err
		}
		messages = []Message{message}
	case "user/input":
		var batch InputBatch
		if err = json.Unmarshal(event.Data, &batch); err != nil {
			return nil, true, err
		}
		if batch.Version != 1 || len(batch.Contexts) == 0 {
			return nil, true, errors.New("invalid input batch version or context")
		}
		messages = append([]Message{batch.Message}, batch.Contexts...)
	default:
		return nil, false, nil
	}
	for _, message := range messages {
		if message.Role != "user" || len(message.Content) == 0 {
			return nil, true, errors.New("input batch requires nonempty user messages")
		}
	}
	return messages, true, nil
}
