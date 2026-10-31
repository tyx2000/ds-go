package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"dsgo/internal/core"
)

func toolContext(t *testing.T) core.ToolContext {
	t.Helper()
	return core.ToolContext{SessionID: "owner", CWD: t.TempDir(), Permission: "workspace-write", ApprovalPolicy: "never"}
}
func call(t *testing.T, r *Registry, tc core.ToolContext, name string, args any) core.ToolResult {
	t.Helper()
	res, err := r.Execute(context.Background(), tc, name, core.JSON(args))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestAtomicEditAndAmbiguity(t *testing.T) {
	r := New()
	defer r.Close()
	tc := toolContext(t)
	call(t, r, tc, "write", map[string]any{"file_path": "dir/test.txt", "content": "one\none\n"})
	p := filepath.Join(tc.CWD, "dir/test.txt")
	if err := os.Chmod(p, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := r.Execute(context.Background(), tc, "edit", core.JSON(map[string]any{"file_path": p, "old_string": "one", "new_string": "two"}))
	if err == nil {
		t.Fatal("ambiguous edit succeeded")
	}
	b, _ := os.ReadFile(p)
	if string(b) != "one\none\n" {
		t.Fatal("ambiguous edit modified file")
	}
	call(t, r, tc, "edit", map[string]any{"file_path": p, "old_string": "one", "new_string": "two", "replace_all": true})
	b, _ = os.ReadFile(p)
	st, _ := os.Stat(p)
	if string(b) != "two\ntwo\n" || st.Mode().Perm() != 0600 {
		t.Fatalf("unexpected edit %q mode %v", b, st.Mode())
	}
}

func TestOutsideAndSymlinkWriteRequireApproval(t *testing.T) {
	r := New()
	defer r.Close()
	tc := toolContext(t)
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(tc.CWD, "link"))
	p := filepath.Join(tc.CWD, "link/escaped")
	if _, err := r.Execute(context.Background(), tc, "write", core.JSON(map[string]any{"file_path": p, "content": "bad"})); err == nil {
		t.Fatal("symlink escaped policy")
	}
	if _, err := os.Stat(filepath.Join(outside, "escaped")); !os.IsNotExist(err) {
		t.Fatal("outside path was written")
	}
	approvals := 0
	tc.ApprovalPolicy = "on-request"
	tc.Approve = func(context.Context, core.Approval) (bool, error) { approvals++; return true, nil }
	call(t, r, tc, "write", map[string]any{"file_path": p, "content": "allowed"})
	if approvals != 1 {
		t.Fatalf("approvals=%d", approvals)
	}
	b, _ := os.ReadFile(filepath.Join(outside, "escaped"))
	if string(b) != "allowed" {
		t.Fatalf("wrong content: %q", b)
	}
}

func TestCanceledApprovalCannotGrantWrite(t *testing.T) {
	r := New()
	defer r.Close()
	tc := toolContext(t)
	tc.Permission = "read-only"
	tc.ApprovalPolicy = "on-request"
	ctx, cancel := context.WithCancel(context.Background())
	tc.Approve = func(context.Context, core.Approval) (bool, error) { cancel(); return true, nil }
	_, err := r.Execute(ctx, tc, "write", core.JSON(map[string]any{"file_path": "no.txt", "content": "no"}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(tc.CWD, "no.txt")); !os.IsNotExist(err) {
		t.Fatal("late approval caused write")
	}
}

func TestReadAndSearchBoundaries(t *testing.T) {
	r := New()
	defer r.Close()
	tc := toolContext(t)
	os.WriteFile(filepath.Join(tc.CWD, "text"), []byte("a\nb\nc\n"), 0600)

	res := call(t, r, tc, "read", map[string]any{"file_path": "text", "offset": 2, "limit": 1})
	if !strings.Contains(res.Content, `"text":"b"`) || strings.Contains(res.Content, `"text":"c"`) {
		t.Fatalf("bad page %s", res.Content)
	}

	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("find-me"), 0600)
	os.Symlink(outside, filepath.Join(tc.CWD, "outside"))
	res = call(t, r, tc, "search_files", map[string]any{"pattern": "find-me"})
	if strings.Contains(res.Content, "secret") {
		t.Fatal("search traversed symlink")
	}
}

func TestArgumentAndPreDispatchCancellation(t *testing.T) {
	r := New()
	defer r.Close()
	tc := toolContext(t)
	for _, input := range []string{`{}`, `{"file_path":1}`, `{"file_path":"a","unknown":1}`, `{"file_path":"a"} {}`} {
		if _, err := r.Execute(context.Background(), tc, "read", json.RawMessage(input)); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Execute(ctx, tc, "write", core.JSON(map[string]any{"file_path": "a", "content": "x"})); !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v", err)
	}
}

func TestShellCancellationTerminatesProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix process group test")
	}
	r := New()
	defer r.Close()
	tc := toolContext(t)
	tc.Permission = "full-access"
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := r.Execute(ctx, tc, "standard_bash", core.JSON(map[string]any{"description": "test", "command": "sleep 30 & wait"}))
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("process group did not terminate promptly")
	}
}

func TestRestrictedBackendEnforcesWrites(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no implemented backend")
	}
	r := New()
	defer r.Close()
	tc := toolContext(t)
	outside := t.TempDir()
	p := filepath.Join(outside, "blocked")
	res, err := r.Execute(context.Background(), tc, "standard_bash", core.JSON(map[string]any{"description": "test", "command": "printf forbidden > '" + p + "'"}))
	if err != nil {
		if strings.Contains(err.Error(), "unavailable") {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	if strings.Contains(res.Content, "sandbox_apply: Operation not permitted") || strings.Contains(res.Content, "Creating new namespace failed") {
		t.Skip("outer sandbox prohibits nested OS sandbox; real enforcement requires an unsandboxed test run")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("sandbox allowed outside write; result=%s", res.Content)
	}
	res = call(t, r, tc, "standard_bash", map[string]any{"description": "test", "command": "printf permitted > inside"})
	b, err := os.ReadFile(filepath.Join(tc.CWD, "inside"))
	if err != nil || string(b) != "permitted" {
		t.Fatalf("workspace write failed: %v %s", err, res.Content)
	}
}

func TestCredentialEnvironmentScrubbing(t *testing.T) {
	clean := processEnvironment([]string{"PATH=/bin", "HOME=/tmp", "OPENAI_API_KEY=hidden", "PASSWORD=hidden", "AWS_SECRET_ACCESS_KEY=hidden", "SESSION_TOKEN=hidden", "DSH_AUTH=hidden", "DSGO_HOME=hidden", "TERM=xterm"})
	if len(clean) != 3 {
		t.Fatalf("unexpected environment %v", clean)
	}
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell test")
	}
	t.Setenv("DSGO_API_KEY", "never-inherit")
	t.Setenv("THIRD_PARTY_TOKEN", "never-inherit")
	t.Setenv("DSH_AUTH", "never-inherit")
	r := New()
	defer r.Close()
	tc := toolContext(t)
	tc.Permission = "full-access"
	res := call(t, r, tc, "standard_bash", map[string]any{"description": "test", "command": `printf '[%s|%s|%s]' "${DSGO_API_KEY-}" "${THIRD_PARTY_TOKEN-}" "${DSH_AUTH-}"`})
	if !strings.Contains(res.Content, "[||]") {
		t.Fatalf("subprocess inherited harness credentials: %s", res.Content)
	}
}

func TestApprovalAuditFailurePreventsWrite(t *testing.T) {
	for _, failAt := range []string{"approval/requested", "approval/settled"} {
		t.Run(failAt, func(t *testing.T) {
			r := New()
			defer r.Close()
			tc := toolContext(t)
			tc.Permission = "read-only"
			tc.ApprovalPolicy = "on-request"
			approved := false
			tc.Approve = func(context.Context, core.Approval) (bool, error) { approved = true; return true, nil }
			tc.Emit = func(kind string, v any) error {
				if kind == failAt {
					return errors.New("disk full")
				}
				return nil
			}
			_, err := r.Execute(context.Background(), tc, "write", core.JSON(map[string]any{"file_path": "never-created", "content": "no"}))
			if err == nil || !strings.Contains(err.Error(), "audit failed") {
				t.Fatalf("unexpected error %v", err)
			}
			if failAt == "approval/requested" && approved {
				t.Fatal("approval reached after failed request audit")
			}
			if _, err := os.Stat(filepath.Join(tc.CWD, "never-created")); !os.IsNotExist(err) {
				t.Fatal("failed audit permitted write")
			}
		})
	}
}
