package cli

import "dsgo/internal/core"

func (a *App) planFor(id string) bool {
	if id == a.current.Load() {
		return a.plan.Load()
	}
	if a.store == nil {
		return a.plan.Load()
	}
	s, err := a.store.Get(id)
	if err != nil {
		return true
	}
	return s.Plan
}
func (a *App) setPlan(id string, on bool) error {
	s, err := a.store.Get(id)
	if err != nil {
		return err
	}
	s.Plan = on
	if on {
		a.registry.InvalidateSession(id)
		a.manager.cancelChildren(id)
		a.manager.invalidateChildren(id)
	}
	if err = a.store.Update(s); err != nil {
		return err
	}
	if id == a.current.Load() {
		a.plan.Store(on)
		a.session.Plan = on
	}
	_, err = a.store.Append(id, "", "plan/updated", map[string]any{"enabled": on})
	return err
}

func inheritPlan(child *core.Session, parent core.Session) { child.Plan = parent.Plan }
