package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"dsgo/internal/core"
	"dsgo/internal/workflow"
)

func workflowApp(t *testing.T, model core.Model) (*App, core.Session) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node unavailable")
	}
	a, parent := lifecycleApp(t, model)
	if a.workflows != nil {
		a.workflows.Close()
	}
	a.workflows = workflow.New(workflow.Options{NodePath: a.cfg.NodePath, Command: func(ctx context.Context, cwd, workspace, mode, node string, args ...string) (*exec.Cmd, func(), error) {
		cmd := exec.CommandContext(ctx, node, args...)
		cmd.Dir = cwd
		return cmd, func() {}, nil
	}, Child: a.workflowChild})
	t.Cleanup(a.workflows.Close)
	return a, parent
}
func workflowInput(script string, background bool) json.RawMessage {
	return core.JSON(map[string]any{"script": script, "meta": map[string]any{"name": "fixture", "description": "fixture workflow"}, "run_in_background": background})
}
func TestWorkflowCreatesAttributedFreshChildrenAndDurableDisplayFacts(t *testing.T) {
	a, parent := workflowApp(t, lifecycleModel(func(ctx context.Context, req core.Request, emit func(core.Delta)) (core.Response, error) {
		return core.Response{Content: []core.Block{{Type: "text", Text: "done"}}}, nil
	}))
	parent.Preset, parent.ExecutionMode = "minimal", "ptc-repl"
	if err := a.store.Update(parent); err != nil {
		t.Fatal(err)
	}
	result, err := a.workflowTool(a.ctx, core.ToolContext{SessionID: parent.ID, CWD: parent.CWD, WorkspaceRoot: parent.CWD, Permission: "read-only"}, workflowInput(`phase('review');return await agent('work',{label:'child',phase:''});`, false))
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err = json.Unmarshal([]byte(result.Content), &value); err != nil {
		t.Fatal(err)
	}
	if value["kind"] != "foreground" || value["result"] != "done" || value["agentsStarted"] != float64(1) {
		t.Fatalf("result %+v", value)
	}
	events, err := a.store.Events(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	var childID string
	for _, event := range events {
		if strings.HasPrefix(event.Type, "tool-workflow/") {
			types = append(types, event.Type)
			if event.Type == "tool-workflow/agent-start" {
				var member map[string]any
				_ = json.Unmarshal(event.Data, &member)
				childID, _ = member["childId"].(string)
				if member["phase"] != "" {
					t.Fatalf("empty phase lost %+v", member)
				}
			}
		}
	}
	if strings.Join(types, ",") != "tool-workflow/run-start,tool-workflow/agent-start,tool-workflow/agent-end,tool-workflow/run-end" {
		t.Fatalf("record protocol %v", types)
	}
	child, err := a.store.Get(childID)
	if err != nil {
		t.Fatal(err)
	}
	if child.ParentID != parent.ID || !child.Delegated || child.Provider != parent.Provider || child.Preset != parent.Preset || child.ExecutionMode != parent.ExecutionMode || child.Permission != parent.Permission {
		t.Fatalf("child inheritance %+v", child)
	}
}
func TestBackgroundWorkflowJobsOwnCancellationAndRestartReplay(t *testing.T) {
	entered := make(chan struct{}, 1)
	a, parent := workflowApp(t, lifecycleModel(func(ctx context.Context, req core.Request, emit func(core.Delta)) (core.Response, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return core.Response{}, ctx.Err()
	}))
	tc := core.ToolContext{SessionID: parent.ID, CWD: parent.CWD, WorkspaceRoot: parent.CWD, Permission: "read-only"}
	result, err := a.workflowTool(a.ctx, tc, workflowInput(`return await agent('wait');`, true))
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	_ = json.Unmarshal([]byte(result.Content), &value)
	id := value["jobId"].(string)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("background child not running")
	}
	other := tc
	other.SessionID = "another"
	if _, handled, err := a.workflowJobs(a.ctx, other, core.JSON(map[string]any{"action": "output", "id": id})); !handled || err == nil {
		t.Fatal("foreign owner read accepted")
	}
	jobs, handled, err := a.workflowJobs(a.ctx, tc, core.JSON(map[string]any{"action": "list"}))
	if !handled || err != nil || !strings.Contains(jobs.Content, id) {
		t.Fatalf("list %s %v", jobs.Content, err)
	}
	killed, handled, err := a.workflowJobs(a.ctx, tc, core.JSON(map[string]any{"action": "kill", "id": id}))
	if !handled || err != nil || !strings.Contains(killed.Content, "cancelled") {
		t.Fatalf("kill %s %v", killed.Content, err)
	}
	a.session = parent
	var out bytes.Buffer
	a.out = &out
	if err = a.workflowCommand(nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "cancelled") {
		t.Fatalf("replay %s", out.String())
	}
}
func TestWorkflowProviderOverrideValidationAndStructuredResults(t *testing.T) {
	a, parent := workflowApp(t, lifecycleModel(func(ctx context.Context, req core.Request, emit func(core.Delta)) (core.Response, error) {
		return core.Response{Content: []core.Block{{Type: "text", Text: `{"count":2}`}}}, nil
	}))
	tc := core.ToolContext{SessionID: parent.ID, CWD: parent.CWD, WorkspaceRoot: parent.CWD, Permission: "read-only"}
	if _, err := a.workflowTool(a.ctx, tc, workflowInput(`return await parallel([()=>agent('work',{provider:'missing'})]);`, false)); err == nil {
		t.Fatal("unknown provider accepted")
	}
	result, err := a.workflowTool(a.ctx, tc, workflowInput(`return await agent('work',{schema:{type:'object',properties:{count:{type:'integer'}},required:['count'],additionalProperties:false}});`, false))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, `"count":2`) {
		t.Fatalf("structured %s", result.Content)
	}
}

func TestBackgroundWorkflowNoticesFollowJobRules(t *testing.T) {
	var parentID string
	a, parent := workflowApp(t, lifecycleModel(func(ctx context.Context, req core.Request, emit func(core.Delta)) (core.Response, error) {
		if req.SessionID == parentID {
			return core.Response{Content: []core.Block{{Type: "text", Text: "parent noted"}}}, nil
		}
		<-ctx.Done()
		return core.Response{}, ctx.Err()
	}))
	parentID = parent.ID
	tc := core.ToolContext{SessionID: parent.ID, CWD: parent.CWD, WorkspaceRoot: parent.CWD, Permission: "read-only"}
	start := func(script string) string {
		t.Helper()
		result, err := a.workflowTool(a.ctx, tc, workflowInput(script, true))
		if err != nil {
			t.Fatal(err)
		}
		var value struct{ JobID string }
		_ = json.Unmarshal([]byte(result.Content), &value)
		return value.JobID
	}
	jobNotices := func() []string {
		messages, _ := a.store.Messages(parent.ID)
		var out []string
		for _, m := range messages {
			if m.Role == "user" && len(m.Content) == 1 && strings.HasPrefix(m.Content[0].Text, "background job ") {
				out = append(out, m.Content[0].Text)
			}
		}
		return out
	}
	id := start(`return 'ok';`)
	settleOfficial(t, a, parent.ID)
	if got := jobNotices(); len(got) != 1 || got[0] != "background job "+id+" (workflow: fixture) finished [status: completed]. Read its output with job_output." {
		t.Fatalf("workflow notice: %q", got)
	}
	id = start(`return 'ok';`)
	if _, err := a.Execute(a.ctx, tc, "job_output", core.JSON(map[string]any{"job_id": id, "wait": true})); err != nil {
		t.Fatal(err)
	}
	id = start(`return await agent('wait');`)
	if _, err := a.Execute(a.ctx, tc, "job_kill", core.JSON(map[string]any{"job_id": id})); err != nil {
		t.Fatal(err)
	}
	settleOfficial(t, a, parent.ID)
	if got := jobNotices(); len(got) != 1 {
		t.Fatalf("collected or killed workflows notified: %q", got)
	}
}
