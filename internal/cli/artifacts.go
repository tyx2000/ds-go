package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"dsgo/internal/agent"
	"dsgo/internal/artifacts"
	"dsgo/internal/core"
	"dsgo/internal/tools"
)

type workspaceCaptureKey struct {
	app           *App
	session, turn string
}

var workspaceCaptures sync.Map

func (a *App) artifactStore() (*artifacts.Store, error) { return artifacts.Open(a.store.Root()) }

func (a *App) artifactWarning(err error) {
	if a.err == nil {
		return
	}
	a.errorLine("dsgo: artifact preview unavailable: " + clean(err.Error()))
}

func (a *App) artifactTurn(sessionID, turnID string) (int, string, error) {
	events, err := a.store.Events(sessionID)
	if err != nil {
		return 0, "", err
	}
	turn := 0
	callID := ""
	found := false
	for _, e := range events {
		if e.Type == "turn/started" {
			turn++
			if e.TurnID == turnID {
				found = true
			}
		}
		if e.TurnID == turnID && (e.Type == "tool/started" || e.Type == "tool/subcall") {
			var p struct{ ID, Name string }
			if json.Unmarshal(e.Data, &p) == nil && p.Name == "present" {
				callID = p.ID
			}
		}
		if found && e.TurnID != turnID && e.Type == "turn/started" {
			return turn - 1, callID, nil
		}
	}
	if !found {
		return 0, "", errors.New("present requires an open agent turn")
	}
	return turn, callID, nil
}

func (a *App) registerArtifactTools() error {
	return a.registry.Register(toolSpec("present", "Declare existing files as final deliverables for the user. Use it when the user needs a separate file, especially Office documents, spreadsheets, and slide decks; prefer your final response when that suffices. The user opens the current files; their contents are not copied.", `{"type":"object","properties":{"files":{"type":"array","description":"Usually the 1-2 most important deliverables; at most 4 per call.","minItems":1,"maxItems":8,"items":{"type":"object","additionalProperties":false,"properties":{"path":{"type":"string","description":"Path of an existing regular file. Relative paths use the Session working directory."},"description":{"type":"string","description":"Brief description for the user."}},"required":["path"]}}},"required":["files"],"additionalProperties":false}`), func(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
		if tc.TurnID == "" || tc.SessionID == "" || tc.Emit == nil {
			return core.ToolResult{}, errors.New("present requires an open agent turn and durable event channel")
		}
		var p struct {
			Files []artifacts.PresentedFile `json:"files"`
		}
		if err := agent.Decode(raw, &p); err != nil {
			return core.ToolResult{}, err
		}
		files, err := artifacts.ResolvePresented(ctx, tc.CWD, p.Files)
		if err != nil {
			return core.ToolResult{}, err
		}
		turn, callID, err := a.artifactTurn(tc.SessionID, tc.TurnID)
		if err != nil {
			return core.ToolResult{}, err
		}
		if tc.CallID != "" {
			callID = tc.CallID
		}
		if err = ctx.Err(); err != nil {
			return core.ToolResult{}, err
		}
		value := struct {
			Turn  int                       `json:"turn"`
			Files []artifacts.PresentedFile `json:"files"`
		}{turn, files}
		if err = tc.Emit("deliverables/presented", map[string]any{"turn": turn, "callId": callID, "files": files}); err != nil {
			return core.ToolResult{}, err
		}
		return toolValue(value), nil
	})
}

// workspaceTurnStarted/Finished are engine lifecycle hooks. Engine session locks
// serialize one session's snapshots; delegated sessions are intentionally skipped.
func (a *App) workspaceTurnStarted(ctx context.Context, session core.Session, turnID string) error {
	if session.Delegated {
		return nil
	}
	s, err := a.artifactStore()
	if err != nil {
		a.artifactWarning(err)
		return nil
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	before, err := s.Capture(bounded, session.CWD)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		a.artifactWarning(err)
		return nil
	}
	workspaceCaptures.Store(workspaceCaptureKey{a, session.ID, turnID}, before)
	return nil
}
func (a *App) workspaceTurnFinished(ctx context.Context, session core.Session, turnID string) error {
	key := workspaceCaptureKey{a, session.ID, turnID}
	value, ok := workspaceCaptures.LoadAndDelete(key)
	if !ok {
		return nil
	}
	s, err := a.artifactStore()
	if err != nil {
		a.artifactWarning(err)
		return nil
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	after, err := s.Capture(bounded, session.CWD)
	if err != nil {
		a.artifactWarning(err)
		return nil
	}
	turn, _, err := a.artifactTurn(session.ID, turnID)
	if err != nil {
		return err
	}
	changes, err := s.Compare(bounded, value.(artifacts.Snapshot), after, turn)
	if err != nil {
		a.artifactWarning(err)
		return nil
	}
	if changes.Summary.Total == 0 {
		return nil
	}
	if err = s.SaveChanges(session.ID, turnID, changes); err != nil {
		a.artifactWarning(err)
		return nil
	}
	_, err = a.store.Append(session.ID, turnID, "workspace/changes", map[string]any{"turn": turn})
	return err
}

// spillToolResult is for the model-facing top-level copy only, after canonical
// tool execution has settled. Nested PTC values must never pass through it.
func (a *App) spillToolResult(ctx context.Context, tc core.ToolContext, name string, result core.ToolResult) (out core.ToolResult, err error) {
	defer func() { out, err = a.finishJobOutputBound(tc, name, out, err) }()
	if !result.IsError {
		if rendered, ok := tools.RenderOfficialFileResult(name, result.Content); ok {
			result.Meta = json.RawMessage(result.Content)
			result.Content = rendered
			return result, nil
		}
	}
	// Shell values render first, then pass through the same output retention.
	if !result.IsError {
		if rendered, ok := tools.RenderShellResult(name, result.Content); ok {
			result.Meta = json.RawMessage(result.Content)
			result.Content = rendered
		} else if p, ok := result.Meta.(presentation); ok {
			result.Meta = json.RawMessage(result.Content)
			result.Content = p.text
		}
	}
	if ctx.Err() != nil {
		return result, nil
	}
	if name == "present" && !result.IsError {
		var value struct {
			Files []artifacts.PresentedFile `json:"files"`
		}
		if json.Unmarshal([]byte(result.Content), &value) == nil {
			var lines []string
			for _, file := range value.Files {
				lines = append(lines, "Presented "+file.Path)
			}
			result.Content = strings.Join(lines, "\n")
			return result, nil
		}
	}
	s, err := a.artifactStore()
	if err != nil {
		a.artifactWarning(err)
		return result, nil
	}
	text, ref, err := s.Retain(tc.SessionID, tc.TurnID, name, result.Content, 50000)
	if err != nil {
		a.artifactWarning(err)
		return result, nil
	}
	if ref != nil && tc.Emit != nil {
		if err = tc.Emit("spill/saved", ref); err != nil {
			return result, err
		}
	}
	result.Content = text
	return result, nil
}

func (a *App) artifactCommand(args []string) error {
	if a.session.ID == "" {
		return errors.New("select a session first")
	}
	command := "list"
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}
	s, err := a.artifactStore()
	if err != nil {
		return err
	}
	if command == "spill" {
		if len(args) < 1 || len(args) > 3 {
			return errors.New("usage: /artifacts spill ID [byte-offset] [byte-limit]")
		}
		offset, limit := 0, 65536
		if len(args) > 1 {
			offset, err = strconv.Atoi(args[1])
			if err != nil {
				return err
			}
		}
		if len(args) > 2 {
			limit, err = strconv.Atoi(args[2])
			if err != nil {
				return err
			}
		}
		ref, text, next, err := s.ReadSpill(a.session.ID, args[0], offset, limit)
		if err != nil {
			return err
		}
		a.print(map[string]any{"spill": ref, "offset": offset, "next_offset": next, "content": text})
		return nil
	}
	events, err := a.store.Events(a.session.ID)
	if err != nil {
		return err
	}
	if command == "list" {
		out := []map[string]any{}
		for _, event := range events {
			if event.Type == "deliverables/presented" || event.Type == "workspace/changes" || event.Type == "spill/saved" {
				var value any
				if json.Unmarshal(event.Data, &value) != nil {
					continue
				}
				out = append(out, map[string]any{"seq": event.Seq, "type": event.Type, "turn_id": event.TurnID, "data": value})
			}
		}
		a.print(out)
		return nil
	}
	if command != "changes" && command != "diff" {
		return errors.New("usage: /artifacts [list|changes [TURN_ID]|diff TURN_ID INDEX|spill ID [OFFSET] [LIMIT]]")
	}
	turnID := ""
	if len(args) > 0 {
		turnID = args[0]
	}
	if turnID == "" {
		for i := len(events) - 1; i >= 0; i-- {
			if events[i].Type == "workspace/changes" {
				turnID = events[i].TurnID
				break
			}
		}
	}
	if turnID == "" {
		return errors.New("no recorded workspace changes in this session")
	}
	changes, err := s.Changes(a.session.ID, turnID)
	if err != nil {
		return err
	}
	if command == "changes" {
		a.print(changes.Summary)
		return nil
	}
	if len(args) != 2 {
		return errors.New("usage: /artifacts diff TURN_ID INDEX (zero-based)")
	}
	index, err := strconv.Atoi(args[1])
	if err != nil {
		return err
	}
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	diff, err := s.Diff(ctx, changes, index)
	if err != nil {
		return err
	}
	if a.flags.json {
		a.print(diff)
		return nil
	}
	if diff.Kind != "text" {
		fmt.Fprintf(a.out, "%s: %s\n", clean(diff.Display), diff.Kind)
		return nil
	}
	fmt.Fprintf(a.out, "--- %s (before)\n+++ %s (after)\n", clean(diff.Display), clean(diff.Display))
	for _, h := range diff.Hunks {
		fmt.Fprintf(a.out, "@@ -%d,%d +%d,%d @@\n", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
		for _, line := range h.Lines {
			fmt.Fprintln(a.out, clean(strings.TrimSuffix(line, "\r")))
		}
	}
	return nil
}
