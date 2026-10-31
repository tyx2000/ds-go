package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"

	"dsgo/internal/agent"
	instructions "dsgo/internal/context"
	"dsgo/internal/core"
)

// Sessions carry the official agent-instructions baseline (packages/context/agent-instructions): the user-global and project
// AGENTS.md chain is rendered into a durable user message before a request.
// A visible baseline is reusable only for the same working directory. Surface
// replacement (including compaction) must not make the process cache authoritative.
type instructionBaselines struct {
	mu      sync.Mutex
	loader  *instructions.InstructionLoader
	checked map[string]instructionBaselineCheck
}

type instructionBaselineCheck struct{ CWD, Identity, Digest string }

type baselineSource struct {
	Kind, Form, Identity, Digest, CWD string
}

func (a *App) ensureInstructionBaseline(ctx context.Context, s core.Session, turn string) error {
	b := &a.instructionState
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.loader == nil {
		config := instructions.DefaultInstructionConfig()
		config.HarnessHome = a.store.Root()
		loader, err := instructions.NewInstructionLoader(config)
		if err != nil {
			// A provider failure adds no message; the turn continues without it.
			a.errorLine("agent instructions unavailable: " + err.Error())
			return nil
		}
		b.loader = loader
	}
	previous, err := a.latestBaseline(s.ID)
	if err != nil {
		return err
	}
	cwd, err := a.activeWorkingDirectory(s.ID)
	if err != nil {
		return err
	}
	root, err := b.loader.FindProjectRoot(ctx, cwd)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		a.errorLine("agent instructions unavailable: " + err.Error())
		return nil
	}
	identity := b.loader.BaselineIdentity(cwd, root)
	if previous != nil && previous.CWD == cwd && previous.Identity == identity {
		if checked, ok := b.checked[s.ID]; ok && checked == (instructionBaselineCheck{cwd, identity, previous.Digest}) {
			return nil
		}
	}
	current, err := b.loader.LoadBaseline(ctx, cwd, false)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		a.errorLine("agent instructions unavailable: " + err.Error())
		return nil
	}
	digest := ""
	if current != nil {
		digest = baselineDigest(current.Rendered.Text)
	}
	if b.checked == nil {
		b.checked = map[string]instructionBaselineCheck{}
	}
	if previous == nil && current == nil {
		// An empty chain contributes nothing.
		delete(b.checked, s.ID)
		return nil
	}
	if previous != nil && previous.Digest == digest && previous.CWD == cwd && previous.Identity == identity {
		b.checked[s.ID] = instructionBaselineCheck{cwd, identity, digest}
		return nil
	}
	set := current
	if previous != nil {
		if set, err = b.loader.LoadBaseline(ctx, cwd, true); err != nil || set == nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
	message := core.TextMessage("user", set.Rendered.Text)
	files := make([]map[string]string, 0, len(set.Included))
	for _, file := range set.Included {
		files = append(files, map[string]string{"absolutePath": file.AbsolutePath, "version": file.Version})
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	message.Source = map[string]any{"kind": "agent-instructions", "form": "baseline", "identity": identity, "cwd": cwd, "digest": digest, "files": files}
	event, err := a.store.Append(s.ID, turn, "user/message", message)
	if err != nil {
		return err
	}
	a.notice(agent.Notice{SessionID: s.ID, Type: "event", Data: event})
	b.checked[s.ID] = instructionBaselineCheck{cwd, identity, digest}
	return nil
}

func (a *App) latestBaseline(id string) (*baselineSource, error) {
	messages, err := a.store.Messages(id)
	if err != nil {
		return nil, err
	}
	var latest *baselineSource
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		var source baselineSource
		data, err := json.Marshal(message.Source)
		if err == nil && json.Unmarshal(data, &source) == nil && source.Kind == "agent-instructions" && source.Form == "baseline" {
			latest = &source
		}
	}
	return latest, nil
}

// baselineDigest identifies the first-load rendering, which replacement
// messages record too, so an unchanged chain is never re-sent.
func baselineDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
