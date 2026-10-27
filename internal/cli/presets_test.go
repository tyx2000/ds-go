//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package cli

import (
	"context"
	"strings"
	"testing"

	"dsgo/internal/core"
)

func TestPresetsAdvertiseOfficialBashOutsideMinimal(t *testing.T) {
	a, parent := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		return core.Response{}, nil
	}))
	parent.Permission = "full-access"
	if err := a.store.Update(parent); err != nil {
		t.Fatal(err)
	}
	specs := func() map[string]core.ToolSpec {
		out := map[string]core.ToolSpec{}
		for _, spec := range a.specsFor(parent) {
			out[spec.Name] = spec
		}
		return out
	}
	listed := specs()
	for _, name := range []string{"bash", "job_output", "job_list", "job_kill"} {
		if _, ok := listed[name]; !ok {
			t.Fatalf("standard preset does not list %s", name)
		}
	}
	for _, name := range []string{"run_shell", "jobs", "terminal", "standard_bash"} {
		if _, ok := listed[name]; ok {
			t.Fatalf("standard preset lists %s", name)
		}
	}
	if !strings.Contains(string(listed["bash"].InputSchema), `"run_in_background"`) {
		t.Fatalf("bash is not the official schema: %s", listed["bash"].InputSchema)
	}
	tc := core.ToolContext{SessionID: parent.ID, Permission: "full-access", ApprovalPolicy: "never"}
	run := func(name string, args map[string]any) string {
		t.Helper()
		res, err := a.Execute(a.ctx, tc, name, core.JSON(args))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res, err = a.spillToolResult(a.ctx, tc, name, res); err != nil {
			t.Fatal(err)
		}
		return res.Content
	}
	if got := run("bash", map[string]any{"description": "Greet", "command": "printf hi; exit 2"}); got != "hi\n[exit code: 2]" {
		t.Fatalf("official bash: %q", got)
	}
	if got := run("job_list", map[string]any{}); got != "(no background jobs)" {
		t.Fatalf("job_list: %q", got)
	}
	// Transcripts saved before the switch still replay dsgo's earlier shell tool.
	if got := run("run_shell", map[string]any{"command": "printf replay"}); !strings.Contains(got, "replay") {
		t.Fatalf("run_shell replay: %q", got)
	}
	a.plan.Store(true)
	if _, err := a.Execute(a.ctx, tc, "bash", core.JSON(map[string]any{"description": "Widen", "command": "true", "sandbox_permissions": "danger-full-access", "justification": "test"})); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("plan mode escalation: %v", err)
	}
	a.plan.Store(false)

	parent.Preset = "minimal"
	if err := a.store.Update(parent); err != nil {
		t.Fatal(err)
	}
	listed = specs()
	if _, ok := listed["job_list"]; ok || strings.Contains(string(listed["bash"].InputSchema), `"description"`) {
		t.Fatalf("minimal preset tools: %v", listed)
	}
	if got := run("bash", map[string]any{"command": "printf kept"}); !strings.Contains(got, "kept\n[Command finished with exit code 0]") {
		t.Fatalf("minimal persistent bash: %q", got)
	}
	if _, err := a.Execute(a.ctx, tc, "job_list", core.JSON(map[string]any{})); err == nil {
		t.Fatal("minimal preset accepted job_list")
	}
}

func TestJobNoticesWakeTheOwningSession(t *testing.T) {
	a, parent, _ := officialApp(t)
	tc := core.ToolContext{SessionID: parent.ID, Permission: "full-access", ApprovalPolicy: "never"}
	run := func(name string, args map[string]any) string {
		t.Helper()
		res, err := a.Execute(a.ctx, tc, name, core.JSON(args))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res, err = a.spillToolResult(a.ctx, tc, name, res); err != nil {
			t.Fatal(err)
		}
		return res.Content
	}
	jobNotices := func() []string {
		messages, err := a.store.Messages(parent.ID)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range messages {
			if m.Role == "user" && len(m.Content) == 1 && strings.HasPrefix(m.Content[0].Text, "background job ") {
				out = append(out, m.Content[0].Text)
			}
		}
		return out
	}
	id := strings.TrimPrefix(run("bash", map[string]any{"description": "Greet later", "command": "sleep 0.1; printf hi", "run_in_background": true}), "started background job ")
	// The one-shot run path waits for the job, its notice and the woken turn.
	settleOfficial(t, a, parent.ID)
	got := jobNotices()
	if len(got) != 1 || got[0] != "background job "+id+" (bash: sleep 0.1; printf hi) finished [status: completed, exit code: 0]. Read its output with job_output." {
		t.Fatalf("job notice: %q", got)
	}
	messages, _ := a.store.Messages(parent.ID)
	if last := messages[len(messages)-1]; last.Role != "assistant" || last.Content[0].Text != "parent noted" {
		t.Fatalf("owner was not woken: %+v", last)
	}
	// Collecting the job with a blocking read leaves nothing to notify.
	id = strings.TrimPrefix(run("bash", map[string]any{"description": "Wait here", "command": "sleep 0.2", "run_in_background": true}), "started background job ")
	if text := run("job_output", map[string]any{"job_id": id, "wait": true}); !strings.HasSuffix(text, "[status: completed, exit code: 0]") {
		t.Fatalf("collected read: %q", text)
	}
	settleOfficial(t, a, parent.ID)
	if got = jobNotices(); len(got) != 1 {
		t.Fatalf("awaited job notified: %q", got)
	}
}
