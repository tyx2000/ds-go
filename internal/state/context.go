package state

import "dsgo/internal/core"

func (s *Store) ContextSurface(id string) ([]core.ContextNode, error) {
	events, err := s.Events(id)
	if err != nil {
		return nil, err
	}
	return core.ContextSurface(events)
}

// AppendContextReplacement validates the exact current surface under the same
// process/file lock that appends and fsyncs the replacement. A stale caller
// publishes nothing, and a torn final record cannot partially rewrite history.
func (s *Store) AppendContextReplacement(id, turn string, replacement core.ContextReplacement) (event core.Event, err error) {
	err = s.withLock(func() error {
		events, err := s.events(id)
		if err != nil {
			return err
		}
		nodes, err := core.ContextSurface(events)
		if err != nil {
			return err
		}
		if err = core.ValidateContextReplacement(nodes, replacement); err != nil {
			return err
		}
		event, err = s.append(id, turn, "context/replace", replacement)
		return err
	})
	return
}
