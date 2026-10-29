package state

import "dsgo/internal/core"

// AppendInputBatch publishes the readable prompt and immutable source captures
// at the same append/fsync boundary. A torn last record publishes neither.
func (s *Store) AppendInputBatch(id, turn string, message core.Message, contexts []core.Message) (core.Event, error) {
	if len(contexts) == 0 {
		return s.Append(id, turn, "user/message", message)
	}
	batch := core.InputBatch{Version: 1, Message: message, Contexts: contexts}
	if _, _, err := core.UserInputMessages(core.Event{Type: "user/input", Data: core.JSON(batch)}); err != nil {
		return core.Event{}, err
	}
	return s.Append(id, turn, "user/input", batch)
}

// CompactionBoundary keeps the latest direct input and all of its captured
// context together, even though Messages expands them into separate messages.
func (s *Store) CompactionBoundary(id string) (int, error) {
	nodes, err := s.ContextSurface(id)
	if err != nil {
		return 0, err
	}
	boundary := 0
	for index, node := range nodes {
		if node.InputGroup != 0 && (index == 0 || nodes[index-1].InputGroup != node.InputGroup) {
			boundary = index
		}
	}
	return boundary, nil
}
