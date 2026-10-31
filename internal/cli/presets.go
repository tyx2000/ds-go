package cli

import (
	"context"
	"fmt"
	"strings"

	"dsgo/internal/core"
)

// specsFor lists the tools the model sees. The fresh-shell bash is
// registered as standard_bash and advertised under its official name.
func (a *App) specsFor(s core.Session) []core.ToolSpec {
	all := a.registry.Specs()
	filtered := make([]core.ToolSpec, 0, len(all))
	for _, spec := range all {
		switch spec.Name {
		case "standard_bash":
			spec.Name = "bash"
		case "subagent":
			// Child LLM selection appears only for a Session with a recorded policy.
			spec = a.subagentSpecFor(s, false)
		case "list_subagent_models":
			if policy, err := a.subagentPolicy(s.ID); err != nil || policy == nil {
				continue
			}
		}
		filtered = append(filtered, spec)
	}
	return filtered
}

func (a *App) requestContext(ctx context.Context, s core.Session) ([]core.Message, error) {
	cwd, err := a.ensureWorkingDirectory(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	parts := []string{workingDirectoryContext(cwd)}
	if s.Delegated {
		parts = append(parts, "You are a delegated subagent: your permission scope was fixed when you were started and cannot be widened from inside this session — operations that require approval are rejected automatically. When the task needs access beyond that scope, do not retry the denied operation; state the limitation in your reply so the delegating agent can handle it.")
	}
	depth, err := a.subagentDepth(s.ID)
	if err != nil {
		return nil, err
	}
	cap := a.subagentMaxDepth()
	if depth >= cap {
		for _, spec := range a.specsFor(s) {
			if spec.Name == "subagent" || spec.Name == "subagent_fork" {
				parts = append(parts, fmt.Sprintf("Delegation depth for `%s`: current %d, maximum %d, remaining levels 0. Do not call `%s` at this depth; carry out the work directly.", spec.Name, depth, cap, spec.Name))
			}
		}
	}
	return []core.Message{core.TextMessage("user", strings.Join(parts, "\n\n"))}, nil
}
