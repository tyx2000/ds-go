package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"dsgo/internal/core"
)

func TestJobOutputBoundRetainsUTF8StatusAndCanonicalValue(t *testing.T) {
	text := strings.Repeat("旧", 70) + "新内容\n"
	canonical := core.JSON(map[string]any{"text": text, "job": publicJob{ID: "a", Status: "completed", Detail: "exit code: 0"}})
	rendered, ok := RenderShellResult("job_output", string(canonical))
	if !ok {
		t.Fatal("canonical output did not render")
	}
	bounded := BoundJobOutput(rendered, canonical, 80)
	if len(bounded) > 80 || !utf8.ValidString(bounded) || !strings.HasSuffix(bounded, "\n[output truncated]\n[status: completed, exit code: 0]") || !strings.Contains(bounded, "新内容") {
		t.Fatalf("invalid retained output (%d bytes): %q", len(bounded), bounded)
	}
	if BoundJobOutput(rendered, canonical, len(rendered)) != rendered || BoundJobOutput(rendered, canonical, 0) != rendered {
		t.Fatal("fitting or unbounded output changed")
	}
	if !json.Valid(canonical) || !strings.Contains(string(canonical), text[:len(text)-1]) {
		t.Fatal("canonical output was changed")
	}
	replacement := strings.Repeat("policy message ", 20)
	bounded = BoundJobOutput(replacement, canonical, 70)
	if !strings.HasSuffix(bounded, "\n[result truncated]") || strings.Contains(bounded, "status:") {
		t.Fatalf("policy replacement reused canonical status: %q", bounded)
	}
}

func TestJobBoundExistingMarkerAndSmallUTF8Limits(t *testing.T) {
	text := strings.Repeat("x", 100) + "\n[result truncated]"
	got := BoundJobText(text, 30)
	if len(got) > 30 || strings.Count(got, "[result truncated]") != 1 {
		t.Fatalf("marker duplicated: %q", got)
	}
	for limit := 1; limit <= 40; limit++ {
		for _, got := range []string{retainJobHead("你好世界", limit), retainJobTail("你好世界", limit), BoundJobText(strings.Repeat("好", 30), limit)} {
			if len(got) > limit || !utf8.ValidString(got) {
				t.Fatalf("limit %d produced invalid UTF-8: %q", limit, got)
			}
		}
	}
}

func TestJobCompletionBoundPreservesActionableIDOrRecovery(t *testing.T) {
	id := "12345678-1234-1234-1234-123456789012"
	for _, limit := range []int{1, 7, 8, 40, 41, 64, 68, 70, 100, 1000} {
		n := CompletionNotice(id, "pty", strings.Repeat("终端", 80), "completed", "exit code: 0", limit)
		if len(n.Text) > limit || !utf8.ValidString(n.Text) {
			t.Fatalf("limit %d: %q", limit, n.Text)
		}
		if strings.Contains(n.Text, "job ") && !strings.Contains(n.Text, "job_list") && (!strings.Contains(n.Text, id) || !strings.Contains(n.Text, "job_output")) {
			t.Fatalf("partial id or missing action at %d: %q", limit, n.Text)
		}
	}
	if got := CompletionNotice(id, "pty", "done", "completed", "", 64).Text; got != "background job ended; use job_list for id." {
		t.Fatalf("64-byte recovery: %q", got)
	}
	if got := CompletionNotice("x", "bash", "done", "completed", "").Text; got != "background job x (bash: done) finished [status: completed]. Read its output with job_output." {
		t.Fatalf("unspecified producer limit changed: %q", got)
	}
}

func TestJobCompletionSummaryUsesIndependentUTF16Limit(t *testing.T) {
	n := CompletionNotice("id", "pty", strings.Repeat("终端😀", 100), "completed", "exit code: 0")
	if !strings.Contains(n.Text, strings.Repeat("终端😀", 100)) || !strings.HasSuffix(n.Summary, "…") || len(utf16.Encode([]rune(n.Summary))) != core.ContextSummaryMaxChars {
		t.Fatalf("summary and producer byte cap were conflated: %+v", n)
	}
}

func TestJobOutputLimitIsOwnedAndCanonicalValueStaysFull(t *testing.T) {
	r := New()
	defer r.Close()
	st := &standardBashState{status: "completed", stdout: &streamCapture{}, stderr: &streamCapture{}}
	_, _ = st.stdout.Write([]byte(strings.Repeat("full canonical ", 40)))
	st.stdout.close()
	st.stderr.close()
	j := &job{standard: st, id: "limited", owner: "owner", outputLimitBytes: 64, started: time.Now(), finished: time.Now(), done: make(chan struct{}), cancel: func() {}}
	close(j.done)
	r.jobs[j.id] = j
	if limit, ok := r.JobOutputLimit("owner", j.id); !ok || limit != 64 {
		t.Fatalf("owner limit: %d %v", limit, ok)
	}
	for _, owner := range []string{"", "other"} {
		if _, ok := r.JobOutputLimit(owner, j.id); ok {
			t.Fatal("cap exposed across owners")
		}
	}
	res, err := r.jobOutput(context.Background(), core.ToolContext{SessionID: "owner"}, core.JSON(map[string]string{"job_id": j.id}))
	if err != nil || !json.Valid(res.Value) || string(res.Value) != res.Content || len(res.Value) <= 64 {
		t.Fatalf("canonical output was capped: %s %v", res.Content, err)
	}
	r.removeJob(j.id)
	if _, ok := r.JobOutputLimit("owner", j.id); ok {
		t.Fatal("removed job remained visible")
	}
}
