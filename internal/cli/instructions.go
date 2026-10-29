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

// The standard, ptc and cordis presets carry the official agent-instructions
// baseline (packages/context/agent-instructions): the user-global and project
// AGENTS.md chain is rendered once into a durable user message before the
// first request. Each process reconciles a resumed session once and appends a
// complete replacement when the visible baseline changed.
type instructionBaselines struct {
	mu      sync.Mutex
	loader  *instructions.InstructionLoader
	checked map[string]bool
}

type baselineSource struct {
	Kind, Form, Identity, Digest string
}

func (a *App) ensureInstructionBaseline(ctx context.Context, s core.Session, turn string) error {
	if sessionPreset(s) == "minimal" {
		return nil
	}
	b := &a.instructionState
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.checked[s.ID] {
		return nil
	}
	if b.loader == nil {
		loader, err := instructions.NewInstructionLoader(instructions.DefaultInstructionConfig())
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
	current, err := b.loader.LoadBaseline(ctx, cwd, false)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		a.errorLine("agent instructions unavailable: " + err.Error())
		return nil
	}
	identity, digest := "", ""
	if current != nil {
		identity, digest = current.Identity, baselineDigest(current.Rendered.Text)
	}
	if b.checked == nil {
		b.checked = map[string]bool{}
	}
	if previous == nil && current == nil {
		// An empty chain contributes nothing.
		b.checked[s.ID] = true
		return nil
	}
	if previous != nil && previous.Digest == digest && (current == nil || previous.Identity == identity) {
		b.checked[s.ID] = true
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
	message.Source = map[string]any{"kind": "agent-instructions", "form": "baseline", "identity": set.Identity, "digest": digest, "files": files}
	event, err := a.store.Append(s.ID, turn, "user/message", message)
	if err != nil {
		return err
	}
	a.notice(agent.Notice{SessionID: s.ID, Type: "event", Data: event})
	b.checked[s.ID] = true
	return nil
}

func (a *App) latestBaseline(id string) (*baselineSource, error) {
	events, err := a.store.Events(id)
	if err != nil {
		return nil, err
	}
	var latest *baselineSource
	for _, event := range events {
		if event.Type != "user/message" {
			continue
		}
		var message struct {
			Source baselineSource `json:"source"`
		}
		if json.Unmarshal(event.Data, &message) == nil && message.Source.Kind == "agent-instructions" && message.Source.Form == "baseline" {
			source := message.Source
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
