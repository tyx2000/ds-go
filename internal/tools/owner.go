package tools

import "context"

// HasPendingSessionJobs includes legacy processes and waits for notice handoff,
// so a structured child cannot finish ahead of work it still owns.
func (r *Registry) HasPendingSessionJobs(owner string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, job := range r.jobs {
		if job.owner != owner {
			continue
		}
		select {
		case <-job.done:
		default:
			return true
		}
		if job.noticed != nil {
			select {
			case <-job.noticed:
			default:
				return true
			}
		}
	}
	return false
}

// CancelSessionJobs disposes only this owner's detached processes. Registering
// a wait before cancellation makes the teardown, rather than an automatic
// completion notification, own each live job's settlement.
func (r *Registry) CancelSessionJobs(ctx context.Context, owner string) error {
	r.mu.RLock()
	var owned []*job
	for _, job := range r.jobs {
		if job.owner == owner {
			owned = append(owned, job)
		}
	}
	r.mu.RUnlock()
	for _, job := range owned {
		job.mu.Lock()
		job.waiters++
		job.mu.Unlock()
	}
	defer func() {
		for _, job := range owned {
			job.mu.Lock()
			job.waiters--
			job.mu.Unlock()
		}
	}()
	for _, job := range owned {
		job.requestKill("owner disposed")
	}
	for _, job := range owned {
		select {
		case <-job.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if job.noticed != nil {
			select {
			case <-job.noticed:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return nil
}
