package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"dsgo/internal/core"
)

func sessionPreset(s core.Session) string {
	if s.Preset == "" {
		return "standard"
	}
	return s.Preset
}

func (a *App) sessionMode(s core.Session) string {
	if s.ExecutionMode != "" {
		return s.ExecutionMode
	}
	if sessionPreset(s) == "ptc" {
		return "ptc"
	}
	if a.cfg.ExecutionMode != "" {
		return a.cfg.ExecutionMode
	}
	return "native"
}

func (a *App) specsFor(s core.Session) []core.ToolSpec {
	all := a.registry.Specs()
	filtered := make([]core.ToolSpec, 0, len(all))
	for _, spec := range all {
		if sessionPreset(s) == "minimal" {
			if spec.Name == "bash" || spec.Name == "read_image" {
				filtered = append(filtered, spec)
			}
			continue
		}
		// The other presets use the official fresh-shell bash and job_* tools;
		// the persistent shell, PTY and dsgo's earlier shell tools stay unlisted.
		switch spec.Name {
		case "bash", "terminal", "run_shell", "jobs", "spawn_agent", "wait_agent":
			continue
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
		if spec.Name == "read_file" || spec.Name == "write_file" || spec.Name == "edit_file" {
			continue
		}
		if spec.Name == "run_code" && a.sessionMode(s) == "native" {
			continue
		}
		filtered = append(filtered, spec)
	}
	return filtered
}

func (a *App) ptcInstructions(s core.Session) string {
	mode := a.sessionMode(s)
	if mode == "native" {
		return ""
	}
	var b strings.Builder
	if mode == "ptc-repl" {
		b.WriteString("Execute erasable TypeScript in a retained per-session Node REPL. Top-level await and final expressions are supported; top-level return is not. Memory clears on cancellation, timeout, permission change and close. Await every call.\n")
	} else {
		b.WriteString("Execute erasable TypeScript as an async function body in a fresh Node process. Top-level await and return are supported. Await every tool call.\n")
	}
	for _, spec := range a.specsFor(s) {
		if spec.Name == "run_code" {
			continue
		}
		fmt.Fprintf(&b, "tools[%q](args) // %s\nArguments JSON schema: %s\n", spec.Name, spec.Description, spec.InputSchema)
	}
	return b.String()
}

func (a *App) requestContext(ctx context.Context, s core.Session) ([]core.Message, error) {
	if sessionPreset(s) == "minimal" {
		return nil, nil
	}
	cwd, err := a.ensureWorkingDirectory(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	return []core.Message{core.TextMessage("user", workingDirectoryContext(cwd))}, nil
}

func (a *App) presetCommand(arg string) error {
	if arg == "" {
		a.line("%s\nAvailable: standard, ptc, minimal, cordis\n", sessionPreset(a.session))
		return nil
	}
	if arg != "standard" && arg != "ptc" && arg != "minimal" && arg != "cordis" {
		return errors.New("preset must be standard, ptc, minimal, or cordis")
	}
	if a.engine.Busy(a.session.ID) {
		return errors.New("finish or cancel the active turn before changing preset")
	}
	next := a.session
	next.Preset = arg
	next.ExecutionMode = "native"
	if arg == "ptc" {
		next.ExecutionMode = "ptc"
	}
	if err := a.store.Update(next); err != nil {
		return err
	}
	a.registry.InvalidateSession(next.ID)
	a.manager.cancelChildren(next.ID)
	a.session = next
	return nil
}
