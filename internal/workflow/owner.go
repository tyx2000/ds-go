package workflow

import "context"

// CancelOwner quiesces only runs started by this owner. It does not wait on
// unrelated workflows or the caller's own enclosing run.
func (e *Engine) CancelOwner(ctx context.Context, owner string) error {
	e.mu.Lock()
	var owned []*Run
	for _, run := range e.runs {
		if run.ParentID == owner {
			owned = append(owned, run)
		}
	}
	e.mu.Unlock()
	for _, run := range owned {
		run.Cancel("owner disposed")
	}
	for _, run := range owned {
		if _, err := run.Wait(ctx); err != nil {
			return err
		}
	}
	return nil
}
