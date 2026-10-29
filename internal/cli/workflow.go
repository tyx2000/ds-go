package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"dsgo/internal/agent"
	"dsgo/internal/core"
	"dsgo/internal/tools"
	"dsgo/internal/workflow"
)

func (a *App) registerWorkflowTools() error {
	if a.workflows == nil {
		a.workflows = workflow.New(workflow.Options{NodePath: a.cfg.NodePath, Command: tools.NewSandboxedCommandInWorkspace, Child: a.workflowChild})
	}
	return a.registry.Register(workflow.ToolSpec(), a.workflowTool)
}
func (a *App) workflowTool(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	req, background, err := workflow.Decode(raw)
	if err != nil {
		return core.ToolResult{}, err
	}
	if _, err = a.store.Get(tc.SessionID); err != nil {
		return core.ToolResult{}, errors.New("workflow requires a calling agent")
	}
	req.ParentID, req.CWD, req.WorkspaceRoot, req.Permission = tc.SessionID, tc.CWD, tc.WorkspaceRoot, tc.Permission
	// Recording is an observe-only continuous prefix. Its failure never changes execution.
	var recordMu sync.Mutex
	recording := !tc.Nested
	req.Observe = func(ev workflow.Event) {
		recordMu.Lock()
		defer recordMu.Unlock()
		var data any
		switch ev.Type {
		case "run-start":
			data = map[string]any{"runId": ev.RunID, "name": ev.Name}
		case "agent-start":
			m := map[string]any{"runId": ev.RunID, "seq": ev.Seq, "label": ev.Label, "childId": ev.ChildID}
			if ev.Phase != nil {
				m["phase"] = *ev.Phase
			}
			data = m
		case "agent-end":
			data = map[string]any{"runId": ev.RunID, "seq": ev.Seq, "outcome": ev.Outcome}
		case "run-end":
			data = map[string]any{"runId": ev.RunID, "stopReason": ev.StopReason}
		}
		if data != nil && recording {
			if _, e := a.store.Append(tc.SessionID, tc.TurnID, "tool-workflow/"+ev.Type, data); e != nil {
				recording = false
			}
		}
		a.notice(agent.Notice{SessionID: tc.SessionID, Type: "workflow/" + ev.Type, Data: ev})
	}
	runCtx := ctx
	if background {
		runCtx = a.ctx
	}
	run, err := a.workflows.Start(runCtx, req)
	if err != nil {
		return core.ToolResult{}, err
	}
	if background {
		a.workflowNotices.started(tc.SessionID)
		go func() {
			defer a.workflowNotices.finished(tc.SessionID)
			_, _ = run.Wait(context.Background())
			run.Dispose()
			a.notice(agent.Notice{SessionID: tc.SessionID, Type: "job/completed", Data: run.JobSnapshot(0, false)})
			if a.workflowNotices.collected(run.ID) || a.ctx.Err() != nil {
				return
			}
			job := publicWorkflowJob(run.Snapshot())
			detail, _ := job["detail"].(string)
			n := tools.CompletionNotice(run.ID, "workflow", run.Meta.Name, job["status"].(string), detail)
			n.Owner = tc.SessionID
			a.deliverJobNotice(n)
		}()
		return toolValue(map[string]any{"kind": "background", "jobId": run.ID, "runId": run.ID}), nil
	}
	result, err := run.Wait(ctx)
	if err != nil {
		run.Cancel("parent step aborted")
		_, _ = run.Wait(context.Background())
	}
	run.Dispose()
	if err != nil {
		return core.ToolResult{}, err
	}
	if result.StopReason != "completed" {
		return core.ToolResult{}, fmt.Errorf("workflow run %s: %s", result.StopReason, result.Error)
	}
	// Keep the native result envelope as the transport value, matching the official tool.
	return toolValue(map[string]any{"kind": "foreground", "runId": run.ID, "agentsStarted": result.AgentsStarted, "result": result.Value}), nil
}
func (a *App) workflowChild(ctx context.Context, parentID string, req workflow.ChildRequest, published func(string)) (workflow.ChildResult, error) {
	if err := ctx.Err(); err != nil {
		return workflow.ChildResult{}, err
	}
	parent, err := a.store.Get(parentID)
	if err != nil {
		return workflow.ChildResult{}, err
	}
	if req.Provider != "" && req.Provider != "deepseek-official" {
		if _, ok := a.cfg.ProviderProfiles[req.Provider]; !ok {
			return workflow.ChildResult{}, fmt.Errorf("provider %q is not configured", req.Provider)
		}
	}
	session, err := a.store.CreateChild(parent.CWD, req.Label, parentID)
	if err != nil {
		return workflow.ChildResult{}, err
	}
	session.Provider, session.Model, session.Thinking, session.Permission = parent.Provider, parent.Model, parent.Thinking, parent.Permission
	session.Plan, session.Preset, session.ExecutionMode = parent.Plan, parent.Preset, parent.ExecutionMode
	if req.Provider != "" {
		session.Provider = req.Provider
	}
	if req.Model != "" {
		session.Model = req.Model
	}
	if err = a.store.Update(session); err != nil {
		_ = a.store.Delete(session.ID)
		return workflow.ChildResult{}, err
	}
	cwd, err := a.activeWorkingDirectory(parentID)
	if err != nil {
		_ = a.store.Delete(session.ID)
		return workflow.ChildResult{}, err
	}
	if cwd != parent.CWD {
		if _, err = a.store.Append(session.ID, "", "working-directory/change", map[string]any{"cwd": cwd}); err != nil {
			_ = a.store.Delete(session.ID)
			return workflow.ChildResult{}, err
		}
	}
	if err = ctx.Err(); err != nil {
		_ = a.store.Delete(session.ID)
		return workflow.ChildResult{}, err
	}
	releaseGoalOwnership := a.ownGoalActivation(session.ID, parentID)
	defer releaseGoalOwnership()
	published(session.ID)
	var structured *agent.StructuredRun
	if len(req.Schema) > 0 {
		var release func()
		structured, release, err = a.engine.AttachStructured(session.ID, req.Schema, func() bool { return a.canCompleteWorkflowMember(ctx, session.ID) })
		if err != nil {
			return workflow.ChildResult{}, err
		}
		defer release()
	}
	err = a.engine.Run(ctx, session.ID, core.TextMessage("user", req.Prompt))
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cleanupCancel()
	if cleanupErr := a.disposeOwner(cleanupCtx, session.ID); cleanupErr != nil {
		return workflow.ChildResult{}, errors.Join(err, cleanupErr)
	}
	if a.manager != nil {
		if drainErr := a.manager.cancelAndDrain(cleanupCtx, session.ID); drainErr != nil {
			return workflow.ChildResult{}, errors.Join(err, drainErr)
		}
	}
	if a.registry != nil {
		a.registry.InvalidateSession(session.ID)
	}
	if err != nil {
		return workflow.ChildResult{StopReason: "failed"}, nil
	}
	messages, err := a.store.Messages(session.ID)
	if err != nil {
		return workflow.ChildResult{}, err
	}
	var text strings.Builder
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			for _, block := range messages[i].Content {
				if block.Type == "text" {
					text.WriteString(block.Text)
				}
			}
			break
		}
	}
	result := workflow.ChildResult{Text: text.String(), StopReason: "completed"}
	if structured != nil {
		captured, ok := structured.Captured()
		if !ok {
			return workflow.ChildResult{StopReason: "failed"}, nil
		}
		decoder := json.NewDecoder(bytes.NewReader(captured))
		decoder.UseNumber()
		var value any
		if err = decoder.Decode(&value); err != nil {
			return workflow.ChildResult{StopReason: "failed"}, nil
		}
		if err = workflow.ValidateStructured(req.Schema, value); err != nil {
			return workflow.ChildResult{StopReason: "failed"}, nil
		}
		result.Structured = value
	}
	return result, nil
}

func (a *App) canCompleteWorkflowMember(ctx context.Context, id string) bool {
	if ctx.Err() != nil {
		return false
	}
	owners := []string{id}
	if a.manager != nil {
		if !a.manager.canCompleteStructured(id) {
			return false
		}
		a.manager.mu.Lock()
		owners = append(owners, a.manager.descendantsLocked(id)...)
		a.manager.mu.Unlock()
	}
	for _, owner := range owners {
		if a.registry != nil && a.registry.HasPendingSessionJobs(owner) {
			return false
		}
		if a.workflows != nil && a.workflows.HasPendingOwner(owner) {
			return false
		}
		if a.workflowNotices.busyOwner(owner) {
			return false
		}
	}
	return ctx.Err() == nil
}

// disposeOwner quiesces resources owned by one completed session without
// touching its enclosing workflow, unrelated sessions or the application.
func (a *App) disposeOwner(ctx context.Context, id string) error {
	if a.manager != nil {
		a.manager.pauseWakes(id)
	}
	if a.workflows != nil {
		if err := a.workflows.CancelOwner(ctx, id); err != nil {
			return err
		}
	}
	if a.registry != nil {
		if err := a.registry.CancelSessionJobs(ctx, id); err != nil {
			return err
		}
		a.registry.InvalidateSession(id)
	}
	return nil
}

// workflowJobs delegates shell jobs to the built-in registry and merges workflow jobs.
func (a *App) workflowJobs(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, bool, error) {
	if a.workflows == nil {
		return core.ToolResult{}, false, nil
	}
	var input struct {
		Action, ID string
		Offset     int
		Consume    bool
		TimeoutMS  int `json:"timeout_ms"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return core.ToolResult{}, false, nil
	}
	if input.Action == "list" {
		shells, err := a.registry.Execute(ctx, tc, "jobs", raw)
		if err != nil {
			return core.ToolResult{}, true, err
		}
		var jobs []any
		if err = json.Unmarshal([]byte(shells.Content), &jobs); err != nil {
			return core.ToolResult{}, true, err
		}
		for _, run := range a.workflows.List(tc.SessionID) {
			r, err := a.workflows.Get(tc.SessionID, run.ID)
			if err == nil {
				m := r.JobSnapshot(0, false)
				delete(m, "output")
				jobs = append(jobs, m)
			}
		}
		return toolValue(jobs), true, nil
	}
	if !strings.HasPrefix(input.ID, "workflow-") {
		return core.ToolResult{}, false, nil
	}
	if input.Offset < 0 {
		return core.ToolResult{}, true, errors.New("offset must be nonnegative")
	}
	run, err := a.workflows.Get(tc.SessionID, input.ID)
	if err != nil {
		return core.ToolResult{}, true, err
	}
	switch input.Action {
	case "output":
	case "kill":
		run.Cancel("background workflow job killed")
		if _, err = run.Wait(ctx); err != nil {
			return core.ToolResult{}, true, err
		}
	case "wait":
		wait := 30 * time.Second
		if input.TimeoutMS != 0 {
			if input.TimeoutMS < 1 || input.TimeoutMS > 60000 {
				return core.ToolResult{}, true, errors.New("timeout_ms must be 1..60000")
			}
			wait = time.Duration(input.TimeoutMS) * time.Millisecond
		}
		if err = a.workflowNotices.wait(ctx, run, wait); err != nil {
			return core.ToolResult{}, true, err
		}
	default:
		return core.ToolResult{}, true, errors.New("unknown jobs action")
	}
	return toolValue(run.JobSnapshot(input.Offset, input.Consume)), true, nil
}

// workflowJobNotices applies the tool-jobs delivery rules to background
// workflow runs, whose settlement the registry does not observe.
type workflowJobNotices struct {
	mu      sync.Mutex
	waiters map[string]int
	quiet   map[string]bool // a live wait or the model's own kill already delivered it
	pending int             // background runs whose notice is not handled yet
	owners  map[string]int
}

func (w *workflowJobNotices) started(owner string) {
	w.mu.Lock()
	w.pending++
	if w.owners == nil {
		w.owners = map[string]int{}
	}
	w.owners[owner]++
	w.mu.Unlock()
}
func (w *workflowJobNotices) finished(owner string) {
	w.mu.Lock()
	w.pending--
	w.owners[owner]--
	if w.owners[owner] == 0 {
		delete(w.owners, owner)
	}
	w.mu.Unlock()
}
func (w *workflowJobNotices) busyOwner(owner string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.owners[owner] > 0
}
func (w *workflowJobNotices) busy() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pending > 0
}
func (w *workflowJobNotices) collect(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.quiet == nil {
		w.quiet = map[string]bool{}
	}
	w.quiet[id] = true
}

// collected reports, once, whether the settled run was already delivered.
func (w *workflowJobNotices) collected(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	quiet := w.quiet[id] || w.waiters[id] > 0
	delete(w.quiet, id)
	return quiet
}

// wait counts as a live waiter; a release by settlement marks the run
// collected before the waiter leaves, so the notice check cannot miss it.
func (w *workflowJobNotices) wait(ctx context.Context, run *workflow.Run, d time.Duration) error {
	w.mu.Lock()
	if w.waiters == nil {
		w.waiters = map[string]int{}
	}
	w.waiters[run.ID]++
	w.mu.Unlock()
	timer := time.NewTimer(d)
	defer timer.Stop()
	var err error
	released := false
	select {
	case <-run.Done():
		released = true
	case <-timer.C:
	case <-ctx.Done():
		err = ctx.Err()
	}
	w.mu.Lock()
	if released {
		if w.quiet == nil {
			w.quiet = map[string]bool{}
		}
		w.quiet[run.ID] = true
	}
	if w.waiters[run.ID]--; w.waiters[run.ID] == 0 {
		delete(w.waiters, run.ID)
	}
	w.mu.Unlock()
	return err
}

// deliverJobNotice hands an owned job's completion notice to its Session:
// a busy owner admits it at its next step, an idle one is woken.
func (a *App) deliverJobNotice(n tools.JobNotice) {
	if a.manager == nil {
		return
	}
	message := core.Message{Role: "user", Content: []core.Block{{Type: "text", Text: n.Text}}, Source: map[string]any{"kind": "tool-jobs", "form": "notice", "summary": n.Summary}}
	if _, err := a.deliverJobCompletion(n.Owner, message); err != nil && !errors.Is(err, errManagerClosing) && a.ctx.Err() == nil {
		a.line("[job notice for %s could not be delivered: %s]\n", n.Owner, clean(err.Error()))
	}
}

// workflowJobControls serves background workflow runs through the official
// job_* tools and merges them into job_list; shell jobs stay with the registry.
func (a *App) workflowJobControls(ctx context.Context, tc core.ToolContext, name string, raw json.RawMessage) (core.ToolResult, bool, error) {
	if a.workflows == nil {
		return core.ToolResult{}, false, nil
	}
	if name == "job_list" {
		shells, err := a.registry.Execute(ctx, tc, name, raw)
		if err != nil {
			return core.ToolResult{}, true, err
		}
		var jobs []map[string]any
		if err = json.Unmarshal([]byte(shells.Content), &jobs); err != nil {
			return core.ToolResult{}, true, err
		}
		for _, run := range a.workflows.List(tc.SessionID) {
			jobs = append(jobs, publicWorkflowJob(run))
		}
		return toolValue(jobs), true, nil
	}
	var input struct {
		JobID     string   `json:"job_id"`
		Wait      bool     `json:"wait"`
		TimeoutMS *float64 `json:"timeout_ms"`
		Reason    string   `json:"reason"`
	}
	if err := json.Unmarshal(raw, &input); err != nil || !strings.HasPrefix(input.JobID, "workflow-") {
		return core.ToolResult{}, false, nil
	}
	run, err := a.workflows.Get(tc.SessionID, input.JobID)
	if err != nil {
		return core.ToolResult{}, true, err
	}
	if name == "job_kill" {
		outcome := "already-finished"
		select {
		case <-run.Done():
		default:
			outcome = "cancellation-requested"
			reason := strings.TrimSpace(input.Reason)
			if reason == "" {
				reason = "background workflow job killed"
			}
			a.workflowNotices.collect(run.ID)
			run.Cancel(reason)
		}
		return toolValue(map[string]any{"outcome": outcome, "job": publicWorkflowJob(run.Snapshot())}), true, nil
	}
	if input.Wait {
		wait, err := a.registry.JobWait(input.TimeoutMS)
		if err != nil {
			return core.ToolResult{}, true, err
		}
		if err = a.workflowNotices.wait(ctx, run, wait); err != nil {
			return core.ToolResult{}, true, err
		}
	}
	job := publicWorkflowJob(run.Snapshot())
	text, _ := run.JobSnapshot(0, true)["output"].(string)
	return toolValue(map[string]any{"text": text, "job": job}), true, nil
}

func publicWorkflowJob(run workflow.Snapshot) map[string]any {
	status, detail := run.Status, ""
	switch run.Status {
	case "running", "completed":
	case "cancelled":
		status = "killed"
		if run.Result != nil {
			detail = run.Result.Error
		}
	default:
		status, detail = "failed", run.Status
		if run.Result != nil && run.Result.Error != "" {
			detail = run.Result.Error
		}
	}
	job := map[string]any{"id": run.ID, "kind": "workflow", "label": run.Meta.Name, "status": status, "startedAt": run.StartedAt.UnixMilli()}
	if detail != "" {
		job["detail"] = detail
	}
	if !run.EndedAt.IsZero() {
		job["finishedAt"] = run.EndedAt.UnixMilli()
	}
	return job
}
func (a *App) workflowCommand(args []string) error {
	id := a.session.ID
	if id == "" {
		id = a.flags.resume
	}
	if id == "" {
		return errors.New("select a session with --resume or /resume")
	}
	if len(args) > 0 && args[0] == "cancel" {
		if len(args) != 2 || a.workflows == nil {
			return errors.New("usage: /workflows cancel RUN_ID")
		}
		run, err := a.workflows.Get(id, args[1])
		if err != nil {
			return err
		}
		run.Cancel("user cancelled workflow")
		_, err = run.Wait(a.ctx)
		return err
	}
	// A restart replays durable display records; it never reruns scripts or child work.
	events, err := a.store.Events(id)
	if err != nil {
		return err
	}
	runs := map[string]map[string]any{}
	var order []string
	for _, ev := range events {
		if !strings.HasPrefix(ev.Type, "tool-workflow/") {
			continue
		}
		var data map[string]any
		if err = json.Unmarshal(ev.Data, &data); err != nil {
			return err
		}
		runID, _ := data["runId"].(string)
		if len(args) > 0 && args[0] != runID {
			continue
		}
		switch ev.Type {
		case "tool-workflow/run-start":
			runs[runID] = map[string]any{"id": runID, "name": data["name"], "status": "interrupted", "members": []any{}}
			order = append(order, runID)
		case "tool-workflow/agent-start":
			if r := runs[runID]; r != nil {
				r["members"] = append(r["members"].([]any), data)
			}
		case "tool-workflow/agent-end":
			if r := runs[runID]; r != nil {
				for _, member := range r["members"].([]any) {
					m := member.(map[string]any)
					if m["seq"] == data["seq"] {
						m["outcome"] = data["outcome"]
					}
				}
			}
		case "tool-workflow/run-end":
			if r := runs[runID]; r != nil {
				r["status"] = data["stopReason"]
			}
		}
	}
	if a.workflows != nil {
		for _, live := range a.workflows.List(id) {
			if r := runs[live.ID]; r != nil {
				r["status"] = live.Status
			}
		}
	}
	out := []any{}
	for _, runID := range order {
		r := runs[runID]
		for _, member := range r["members"].([]any) {
			m := member.(map[string]any)
			if _, ok := m["outcome"]; !ok {
				m["outcome"] = "interrupted"
			}
		}
		out = append(out, r)
	}
	a.print(out)
	return nil
}
