//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dsgo/internal/core"
)

func standardCall(t *testing.T, r *Registry, tc core.ToolContext, name string, args map[string]any) (string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := r.Execute(ctx, tc, name, core.JSON(args))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	model := strings.Replace(name, "standard_bash", "bash", 1)
	text, ok := RenderShellResult(model, res.Content)
	if !ok {
		t.Fatalf("%s value did not render: %s", name, res.Content)
	}
	return res.Content, text
}

func TestStandardBashForegroundFreshShellAndWorkdir(t *testing.T) {
	r := New()
	defer r.Close()
	tc := core.ToolContext{SessionID: "one", CWD: t.TempDir(), Permission: "full-access"}
	raw, text := standardCall(t, r, tc, "standard_bash", map[string]any{"description": "Write both streams", "command": "printf out; printf err >&2; exit 3"})
	if text != "out\n[stderr]\nerr\n[exit code: 3]" {
		t.Fatalf("foreground render: %q", text)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(raw), &v); err != nil || v["kind"] != "foreground" || v["exitCode"] != float64(3) || v["cwd"] != tc.CWD {
		t.Fatalf("canonical value: %s", raw)
	}
	if _, text = standardCall(t, r, tc, "standard_bash", map[string]any{"description": "Run nothing", "command": "true"}); text != "(no output)" {
		t.Fatalf("empty success: %q", text)
	}
	standardCall(t, r, tc, "standard_bash", map[string]any{"description": "Set state", "command": "export DSGO_FRESH=1; mkdir sub; cd sub"})
	if _, text = standardCall(t, r, tc, "standard_bash", map[string]any{"description": "Read state", "command": `printf '%s:%s' "${DSGO_FRESH-unset}" "${PWD##*/}"`}); !strings.HasPrefix(text, "unset:") || strings.HasSuffix(text, ":sub") {
		t.Fatalf("shell state persisted: %q", text)
	}
	raw, text = standardCall(t, r, tc, "standard_bash", map[string]any{"description": "Print directory", "command": "pwd -P", "workdir": "sub"})
	if !strings.HasSuffix(text, "/sub\n") || !strings.Contains(raw, `"cwd":"`+filepath.Join(tc.CWD, "sub")+`"`) {
		t.Fatalf("workdir: %q %s", text, raw)
	}
	// A foreground command that settled within its wait never exposes its id.
	if _, text = standardCall(t, r, tc, "job_list", map[string]any{}); text != "(no background jobs)" {
		t.Fatalf("settled foreground stayed listed: %q", text)
	}
}

func TestStandardBashValidationAndEscalation(t *testing.T) {
	r := New()
	defer r.Close()
	tc := core.ToolContext{SessionID: "one", CWD: t.TempDir(), Permission: "full-access", ApprovalPolicy: "never"}
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"description": " ", "command": "true"}, "invalid description: expected a non-empty string"},
		{map[string]any{"description": "x", "command": " "}, "invalid command: expected a non-empty string"},
		{map[string]any{"description": "x", "command": "true", "timeoutMs": 0}, "invalid timeoutMs: expected a positive number, got 0"},
		{map[string]any{"description": "x", "command": "true", "justification": "because"}, "justification is only valid together with sandbox_permissions"},
		{map[string]any{"description": "x", "command": "true", "sandbox_permissions": "workspace-write", "justification": "narrower"}, `is not strictly wider than this call's current "danger-full-access" mode`},
	} {
		_, err := r.Execute(context.Background(), tc, "standard_bash", core.JSON(c.args))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%v: %v", c.args, err)
		}
	}
	// Repeating the effective mode, or a blank reason without a mode, needs no approval.
	standardCall(t, r, tc, "standard_bash", map[string]any{"description": "x", "command": "true", "sandbox_permissions": "danger-full-access"})
	standardCall(t, r, tc, "standard_bash", map[string]any{"description": "x", "command": "true", "justification": "  "})
	tc.Permission, tc.ApprovalPolicy = "read-only", "on-request"
	tc.Approve = func(context.Context, core.Approval) (bool, error) { return false, nil }
	_, err := r.Execute(context.Background(), tc, "standard_bash", core.JSON(map[string]any{"description": "x", "command": "true", "sandbox_permissions": "danger-full-access", "justification": "needs it"}))
	if err == nil || !strings.Contains(err.Error(), "the user rejected escalating this command") {
		t.Fatalf("rejected escalation: %v", err)
	}
	if _, err = r.Execute(context.Background(), tc, "standard_bash", core.JSON(map[string]any{"description": "x", "command": "true", "sandbox_permissions": "danger-full-access"})); err == nil || !strings.Contains(err.Error(), "requires a justification") {
		t.Fatalf("escalation without reason: %v", err)
	}
}

func TestStandardBashBackgroundJobsAndLegacyView(t *testing.T) {
	r := New()
	defer r.Close()
	tc := core.ToolContext{SessionID: "one", CWD: t.TempDir(), Permission: "full-access"}
	raw, text := standardCall(t, r, tc, "standard_bash", map[string]any{"description": "Print twice", "command": "printf first; sleep 0.2; printf second >&2", "run_in_background": true})
	var started struct{ JobID string }
	if err := json.Unmarshal([]byte(raw), &started); err != nil || !strings.HasPrefix(started.JobID, "bash-") || len(started.JobID) != len("bash-")+16 || text != "started background job "+started.JobID {
		t.Fatalf("background start: %q %s", text, raw)
	}
	if _, text = standardCall(t, r, tc, "job_list", map[string]any{}); !strings.HasPrefix(text, started.JobID+" [bash] running — printf first") {
		t.Fatalf("listing: %q", text)
	}
	if _, err := r.Execute(context.Background(), core.ToolContext{SessionID: "two"}, "job_output", core.JSON(map[string]any{"job_id": started.JobID})); err == nil {
		t.Fatal("another session read the job")
	}
	if _, text = standardCall(t, r, tc, "job_output", map[string]any{"job_id": started.JobID, "wait": true, "timeout_ms": 5000}); text != "first\n[stderr]\nsecond\n[status: completed, exit code: 0]" {
		t.Fatalf("collected output: %q", text)
	}
	if _, text = standardCall(t, r, tc, "job_output", map[string]any{"job_id": started.JobID}); text != "(no new output)\n[status: completed, exit code: 0]" {
		t.Fatalf("second read: %q", text)
	}
	if _, text = standardCall(t, r, tc, "job_kill", map[string]any{"job_id": started.JobID}); text != "job "+started.JobID+" had already finished [status: completed, exit code: 0]" {
		t.Fatalf("kill after finish: %q", text)
	}
	// The original jobs tool still sees official bash jobs, with combined output.
	legacy := call(t, r, tc, "jobs", map[string]any{"action": "output", "id": started.JobID})
	var view map[string]any
	if err := json.Unmarshal([]byte(legacy.Content), &view); err != nil || view["kind"] != "bash" || view["job_status"] != "completed" || view["status"] != "finished" || view["output"] != "firstsecond" || view["description"] != "Print twice" {
		t.Fatalf("legacy view: %s", legacy.Content)
	}
}

func TestStandardBashPromotionAndKillReasons(t *testing.T) {
	r := New()
	defer r.Close()
	tc := core.ToolContext{SessionID: "one", CWD: t.TempDir(), Permission: "full-access"}
	raw, text := standardCall(t, r, tc, "standard_bash", map[string]any{"description": "Start slowly", "command": "printf early; sleep 30", "timeoutMs": 300})
	var promoted struct{ JobID string }
	if err := json.Unmarshal([]byte(raw), &promoted); err != nil || !strings.HasPrefix(text, "early\n[still running after 300ms; moved to background job "+promoted.JobID+"]\n") {
		t.Fatalf("promotion: %q", text)
	}
	if _, text = standardCall(t, r, tc, "job_kill", map[string]any{"job_id": promoted.JobID, "reason": "no longer needed"}); text != "requested cancellation of job "+promoted.JobID {
		t.Fatalf("kill: %q", text)
	}
	// The promoted result already consumed "early".
	if _, text = standardCall(t, r, tc, "job_output", map[string]any{"job_id": promoted.JobID, "wait": true}); text != "(no new output)\n[status: killed, no longer needed]" {
		t.Fatalf("killed output: %q", text)
	}

	// A kill from outside a waiting foreground call is reported as stopped.
	done := make(chan string, 1)
	go func() {
		_, text := standardCall(t, r, tc, "standard_bash", map[string]any{"description": "Wait long", "command": "sleep 30", "timeoutMs": 20000})
		done <- text
	}()
	var id string
	for deadline := time.Now().Add(5 * time.Second); id == "" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		raw, _ := standardCall(t, r, tc, "job_list", map[string]any{})
		var jobs []publicJob
		_ = json.Unmarshal([]byte(raw), &jobs)
		for _, j := range jobs {
			if j.Label == "sleep 30" && j.Status == "running" {
				id = j.ID
			}
		}
	}
	if id == "" {
		t.Fatal("foreground command was not listed while running")
	}
	call(t, r, tc, "jobs", map[string]any{"action": "kill", "id": id})
	select {
	case text = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("foreground call did not settle after kill")
	}
	if text != "(no output)\n[stopped: killed through jobs]\n[killed by signal: SIGKILL]" {
		t.Fatalf("stopped foreground: %q", text)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := r.Execute(ctx, tc, "standard_bash", core.JSON(map[string]any{"description": "Abort", "command": "sleep 30"})); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("aborted call: %v", err)
	}
	if _, text = standardCall(t, r, tc, "job_list", map[string]any{}); strings.Contains(text, "running") {
		t.Fatalf("aborted command kept running: %q", text)
	}
}

func TestStandardBashOutputSpillAndUTF8Reads(t *testing.T) {
	r := New()
	defer r.Close()
	spill := t.TempDir()
	r.SetSpillDir(spill)
	tc := core.ToolContext{SessionID: "one", CWD: t.TempDir(), Permission: "full-access"}
	raw, text := standardCall(t, r, tc, "standard_bash", map[string]any{"description": "Print a lot", "command": "head -c 100000 /dev/zero | tr '\\0' x; printf END"})
	var v struct {
		Stdout struct {
			Text      string
			Truncated bool
			SpillPath string
		}
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil || !v.Stdout.Truncated || len(v.Stdout.Text) != standardStreamBytes || !strings.HasSuffix(v.Stdout.Text, "END") {
		t.Fatalf("tail retention: truncated=%v len=%d", v.Stdout.Truncated, len(v.Stdout.Text))
	}
	full, err := os.ReadFile(v.Stdout.SpillPath)
	if err != nil || len(full) != 100003 || filepath.Dir(v.Stdout.SpillPath) != spill {
		t.Fatalf("spill file: %v %d", err, len(full))
	}
	if !strings.HasSuffix(text, "END\n[output truncated; full output: "+v.Stdout.SpillPath+"]") {
		t.Fatalf("truncation notice: %q", text[len(text)-120:])
	}

	s := &streamCapture{}
	_, _ = s.Write([]byte("a\xe4\xbd"))
	if got, next, _ := s.readFrom(0, false); got != "a" || next != 1 {
		t.Fatalf("partial rune was not held back: %q %d", got, next)
	}
	_, _ = s.Write([]byte("\xa0"))
	if got, _, _ := s.readFrom(1, false); got != "你" {
		t.Fatalf("completed rune: %q", got)
	}
	if !deniedBySandbox("macos-seatbelt", "touch: x: Operation not permitted") || deniedBySandbox("linux-bubblewrap", "Operation not permitted") {
		t.Fatal("denial dialect is backend specific")
	}
	if _, ok := RenderShellResult("bash", "hello\n[Command finished with exit code 0]"); ok {
		t.Fatal("persistent bash text was treated as a standard value")
	}
}

func TestStreamCaptureRevokesFullOutputAfterSpillLimit(t *testing.T) {
	s := &streamCapture{dir: t.TempDir()}
	defer s.close()
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	for written := 0; written < standardSpillBytes; written += len(chunk) {
		if n, err := s.Write(chunk); err != nil || n != len(chunk) {
			t.Fatalf("write: %d %v", n, err)
		}
	}
	path := s.path()
	if path == "" {
		t.Fatal("exactly the spill limit should retain its complete output file")
	}
	if info, err := os.Stat(path); err != nil || info.Size() != standardSpillBytes {
		t.Fatalf("complete spill size: %v %v", info, err)
	}
	if _, err := s.Write([]byte("END")); err != nil {
		t.Fatal(err)
	}
	value := s.value()
	if _, ok := value["spillPath"]; ok || s.path() != "" {
		t.Fatal("partial spill must not be advertised as the full output")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial spill should be removed, got %v", err)
	}
	if !strings.HasSuffix(value["text"].(string), "END") || value["truncated"] != true {
		t.Fatalf("tail must survive spill revocation: %v", value["truncated"])
	}
	if _, err := s.Write([]byte("still running")); err != nil || s.path() != "" {
		t.Fatalf("over-limit streams must not reopen a spill: %v", err)
	}
}

func TestStreamCaptureUnavailableSpillRetainsTail(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blocked, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &streamCapture{dir: blocked}
	defer s.close()
	if n, err := s.Write(bytes.Repeat([]byte("a"), standardStreamBytes+1)); err != nil || n != standardStreamBytes+1 {
		t.Fatalf("spill unavailability must not stop process output: %d %v", n, err)
	}
	value := s.value()
	if _, ok := value["spillPath"]; ok || value["truncated"] != true || len(value["text"].(string)) != standardStreamBytes {
		t.Fatal("unavailable spill must leave a correctly marked in-memory tail")
	}
}

func TestJobNoticesSkipWaitsModelKillsAndTeardown(t *testing.T) {
	r := New()
	got := make(chan JobNotice, 16)
	r.SetJobNotifier(func(n JobNotice) { got <- n })
	tc := core.ToolContext{SessionID: "one", CWD: t.TempDir(), Permission: "full-access"}
	start := func(command string, extra map[string]any) string {
		t.Helper()
		args := map[string]any{"description": "Run " + command, "command": command, "run_in_background": true}
		for k, v := range extra {
			args[k] = v
		}
		raw, _ := standardCall(t, r, tc, "standard_bash", args)
		var v struct{ JobID string }
		_ = json.Unmarshal([]byte(raw), &v)
		return v.JobID
	}
	// handled waits until every owned notice is handed over; none may follow.
	handled := func(want int) []JobNotice {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); r.HasPendingJobs("one"); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("job notices were not handled")
			}
		}
		var out []JobNotice
		for len(got) > 0 {
			out = append(out, <-got)
		}
		if len(out) != want {
			t.Fatalf("notices: %+v", out)
		}
		return out
	}
	id := start("printf done", nil)
	if n := handled(1)[0]; n.Owner != "one" || n.Text != "background job "+id+" (bash: printf done) finished [status: completed, exit code: 0]. Read its output with job_output." || n.Summary != "bash printf done [status: completed, exit code: 0]" {
		t.Fatalf("completion notice: %+v", n)
	}
	// A foreground command that settles inside its own wait is the result itself.
	standardCall(t, r, tc, "standard_bash", map[string]any{"description": "Quick", "command": "true"})
	handled(0)
	// A job_output wait released by the settlement collects it.
	id = start("sleep 0.2", nil)
	standardCall(t, r, tc, "job_output", map[string]any{"job_id": id, "wait": true})
	handled(0)
	// The model's own kill is its delivery; a human stop through /jobs is not.
	id = start("sleep 30", nil)
	standardCall(t, r, tc, "job_kill", map[string]any{"job_id": id})
	handled(0)
	id = start("sleep 30", nil)
	call(t, r, tc, "jobs", map[string]any{"action": "kill", "id": id})
	if n := handled(1)[0]; !strings.Contains(n.Text, "[status: killed, killed through jobs]") {
		t.Fatalf("human stop notice: %q", n.Text)
	}
	// A promoted command notifies when it later finishes.
	raw, _ := standardCall(t, r, tc, "standard_bash", map[string]any{"description": "Slow", "command": "sleep 0.3; printf late", "timeoutMs": 50})
	var promoted struct{ JobID string }
	_ = json.Unmarshal([]byte(raw), &promoted)
	if n := handled(1)[0]; !strings.HasPrefix(n.Text, "background job "+promoted.JobID+" (bash: sleep 0.3; printf late) finished [status: completed") {
		t.Fatalf("promoted notice: %q", n.Text)
	}
	start("sleep 30", nil)
	r.Close()
	handled(0)
}
