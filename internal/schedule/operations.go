package schedule

import (
	"context"
	"path/filepath"
	"time"
)

type hostOperationPermit struct{}

// Host delivery and mutations share one FIFO slot. The permit is scoped to an
// admitted callback so its receipt commit does not recursively acquire the slot.
// An advisory lock also orders management performed by another dsgo process.
func hostOperation[T any](ctx context.Context, s *Store, work func(context.Context) (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if ctx.Value(hostOperationPermit{}) == s.root {
		return work(ctx)
	}
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case <-s.operations:
	}
	defer func() { s.operations <- struct{}{} }()
	var release func()
	for {
		var err error
		release, err = daemonLock(filepath.Join(s.root, "host-operation.lock"))
		if err == nil {
			break
		}
		if !lockBusy(err) {
			return zero, err
		}
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	return work(context.WithValue(ctx, hostOperationPermit{}, s.root))
}

func managementOperation[T any](ctx context.Context, s *Store, work func(context.Context) (T, error)) (T, error) {
	return hostOperation(ctx, s, func(ctx context.Context) (T, error) {
		before := s.revision.Load()
		result, err := work(ctx)
		if err == nil && s.revision.Load() != before {
			select {
			case s.managementWake <- struct{}{}:
			default:
			}
		}
		return result, err
	})
}

func (s *Store) Create(t Task) (Task, error) {
	return managementOperation(context.Background(), s, func(context.Context) (Task, error) { return s.createLegacy(t) })
}
func (s *Store) Cancel(id string) error {
	_, err := managementOperation(context.Background(), s, func(context.Context) (struct{}, error) { return struct{}{}, s.cancelLegacy(id) })
	return err
}
func (s *Store) CreateOfficial(ctx context.Context, sessionID string, req CreateRequest) (View, error) {
	requestedAt := time.Now().UTC().Truncate(time.Millisecond)
	task, err := prepareOfficial(sessionID, req, requestedAt)
	if err != nil {
		return View{}, err
	}
	task.ID = id()
	return managementOperation(ctx, s, func(ctx context.Context) (View, error) { return s.createOfficial(ctx, task) })
}
func (s *Store) UpdateOfficial(ctx context.Context, sessionID, id string, expected Record, patch UpdatePatch) (UpdateResult, error) {
	return managementOperation(ctx, s, func(ctx context.Context) (UpdateResult, error) {
		return s.updateOfficial(ctx, sessionID, id, expected, patch)
	})
}
func (s *Store) DeleteOfficial(ctx context.Context, sessionID, id string) (DeleteResult, error) {
	return managementOperation(ctx, s, func(ctx context.Context) (DeleteResult, error) { return s.deleteOfficial(ctx, sessionID, id) })
}
func (s *Store) StopSession(ctx context.Context, sessionID string) error {
	_, err := managementOperation(ctx, s, func(ctx context.Context) (struct{}, error) { return struct{}{}, s.stopSession(ctx, sessionID) })
	return err
}
func (s *Store) Acknowledge(ctx context.Context, sessionID, id, pendingID, messageID string) error {
	_, err := hostOperation(ctx, s, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, s.acknowledge(ctx, sessionID, id, pendingID, messageID)
	})
	return err
}
