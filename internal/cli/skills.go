package cli

import (
	"context"

	instructions "dsgo/internal/context"
)

// skillsFor uses the same current-directory scope for catalog presentation,
// explicit user reads and model tool reads. Session.CWD remains the original
// workspace permission boundary, not a cache of the current directory.
func (a *App) skillsFor(ctx context.Context, sessionID, fallbackCWD string) ([]instructions.Skill, error) {
	if ctx == nil {
		ctx = a.ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cwd := fallbackCWD
	if a.store != nil && sessionID != "" {
		var err error
		cwd, err = a.ensureWorkingDirectory(ctx, sessionID)
		if err != nil {
			return nil, err
		}
	}
	if cwd == "" {
		cwd = a.flags.cwd
	}
	if cwd == "" {
		// Keep explicitly supplied catalogs usable for callers without a
		// Session or filesystem scope (for example embedded/test clients).
		return append([]instructions.Skill(nil), a.skills...), nil
	}
	skills, err := instructions.Skills(cwd)
	if err != nil {
		return nil, err
	}
	return skills, ctx.Err()
}
