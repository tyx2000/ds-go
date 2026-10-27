package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
	call(t, r, tc, "write_file", map[string]any{"path": "dir/test.txt", "content": "one\none\n"})
	p := filepath.Join(tc.CWD, "dir/test.txt")
	if err := os.Chmod(p, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := r.Execute(context.Background(), tc, "edit_file", core.JSON(map[string]any{"path": p, "old_text": "one", "new_text": "two"}))
	if err == nil {
		t.Fatal("ambiguous edit succeeded")
	}
	b, _ := os.ReadFile(p)
	if string(b) != "one\none\n" {
		t.Fatal("ambiguous edit modified file")
	}
	call(t, r, tc, "edit_file", map[string]any{"path": p, "old_text": "one", "new_text": "two", "replace_all": true})
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
	if _, err := r.Execute(context.Background(), tc, "write_file", core.JSON(map[string]any{"path": p, "content": "bad"})); err == nil {
		t.Fatal("symlink escaped policy")
	}
	if _, err := os.Stat(filepath.Join(outside, "escaped")); !os.IsNotExist(err) {
		t.Fatal("outside path was written")
	}
	approvals := 0
	tc.ApprovalPolicy = "on-request"
	tc.Approve = func(context.Context, core.Approval) (bool, error) { approvals++; return true, nil }
	call(t, r, tc, "write_file", map[string]any{"path": p, "content": "allowed"})
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
	_, err := r.Execute(ctx, tc, "write_file", core.JSON(map[string]any{"path": "no.txt", "content": "no"}))
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
	os.WriteFile(filepath.Join(tc.CWD, "binary"), []byte{'a', 0, 'b'}, 0600)
	res := call(t, r, tc, "read_file", map[string]any{"path": "text", "offset": 1, "limit": 1})
	if !strings.Contains(res.Content, `b\n`) {
		t.Fatalf("bad page %s", res.Content)
	}
	if _, err := r.Execute(context.Background(), tc, "read_file", core.JSON(map[string]any{"path": "binary"})); err == nil {
		t.Fatal("binary accepted")
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
	for _, input := range []string{`{}`, `{"path":1}`, `{"path":"a","unknown":1}`, `{"path":"a"} {}`} {
		if _, err := r.Execute(context.Background(), tc, "read_file", json.RawMessage(input)); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Execute(ctx, tc, "write_file", core.JSON(map[string]any{"path": "a", "content": "x"})); !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v", err)
	}
}

func TestJobsOwnerAndOutputCursor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell test")
	}
	r := New()
	defer r.Close()
	tc := toolContext(t)
	tc.Permission = "full-access"
	res := call(t, r, tc, "run_shell", map[string]any{"command": "printf hello", "background": true})
	var data struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(res.Content), &data); err != nil {
		t.Fatal(err)
	}
	call(t, r, tc, "jobs", map[string]any{"action": "wait", "id": data.ID})
	other := tc
	other.SessionID = "intruder"
	if _, err := r.Execute(context.Background(), other, "jobs", core.JSON(map[string]any{"action": "output", "id": data.ID})); err == nil {
		t.Fatal("cross-session job access")
	}
	for i := 0; i < 2; i++ {
		res = call(t, r, tc, "jobs", map[string]any{"action": "output", "id": data.ID})
		if !strings.Contains(res.Content, `"output":"hello"`) {
			t.Fatal("observer consumed output")
		}
	}
	res = call(t, r, tc, "jobs", map[string]any{"action": "output", "id": data.ID, "consume": true})
	if !strings.Contains(res.Content, "hello") {
		t.Fatal("missing consumed output")
	}
	res = call(t, r, tc, "jobs", map[string]any{"action": "output", "id": data.ID, "consume": true})
	if !strings.Contains(res.Content, `"output":""`) {
		t.Fatal("consumption cursor did not advance")
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
	_, err := r.Execute(ctx, tc, "run_shell", core.JSON(map[string]any{"command": "sleep 30 & wait"}))
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("process group did not terminate promptly")
	}
}

func TestPTCTypeScriptAndNestedApproval(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node unavailable")
	}
	if runtime.GOOS == "windows" {
		t.Skip("fd3 unsupported")
	}
	r := New()
	defer r.Close()
	tc := toolContext(t)
	tc.Permission = "full-access"
	res := call(t, r, tc, "run_code", map[string]any{"code": "const x: number = 41; console.log('log'); await tools.write_file({path:'ptc.txt', content:String(x)}); return {answer:x+1};"})
	if res.IsError || !strings.Contains(res.Content, `"answer":42`) || !strings.Contains(res.Content, "log") {
		t.Fatalf("PTC failed %s", res.Content)
	}
	b, _ := os.ReadFile(filepath.Join(tc.CWD, "ptc.txt"))
	if string(b) != "41" {
		t.Fatalf("nested tool didn't execute: %q", b)
	}
	// Keep direct Node access full-access but ask through the same registry pipeline by invoking a separate handler.
	var mu sync.Mutex
	count := 0
	err := r.Register(spec("guarded", "Approval test.", `{"type":"object"}`), func(ctx context.Context, tc core.ToolContext, in json.RawMessage) (core.ToolResult, error) {
		if err := ask(ctx, tc, "guarded", "approve", in); err != nil {
			return core.ToolResult{}, err
		}
		return result("ok"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	tc.ApprovalPolicy = "on-request"
	tc.Approve = func(context.Context, core.Approval) (bool, error) { mu.Lock(); count++; mu.Unlock(); return false, nil }
	res = call(t, r, tc, "run_code", map[string]any{"code": "return await tools.guarded({});"})
	if !res.IsError || !strings.Contains(res.Content, "permission denied") {
		t.Fatalf("approval bypass: %s", res.Content)
	}
	if count != 1 {
		t.Fatalf("approval count %d", count)
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
	res, err := r.Execute(context.Background(), tc, "run_shell", core.JSON(map[string]any{"command": "printf forbidden > '" + p + "'"}))
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
	res = call(t, r, tc, "run_shell", map[string]any{"command": "printf permitted > inside"})
	b, err := os.ReadFile(filepath.Join(tc.CWD, "inside"))
	if err != nil || string(b) != "permitted" {
		t.Fatalf("workspace write failed: %v %s", err, res.Content)
	}
}

func TestPersistentREPLRetainsMemoryAndClearsOnCancellation(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node unavailable")
	}
	if runtime.GOOS == "windows" {
		t.Skip("fd3 unsupported")
	}
	r := New()
	defer r.Close()
	if err := r.SetPTCMode("persistent-in-memory"); err != nil {
		t.Fatal(err)
	}
	tc := toolContext(t)
	tc.Permission = "full-access"
	res := call(t, r, tc, "run_code", map[string]any{"code": "let retained: number = 41; retained"})
	if res.IsError || !strings.Contains(res.Content, `"value":41`) {
		t.Fatalf("first evaluation: %s", res.Content)
	}
	res = call(t, r, tc, "run_code", map[string]any{"code": "retained += 1; await tools.write_file({path:'persistent.txt',content:String(retained)}); retained"})
	if res.IsError || !strings.Contains(res.Content, `"value":42`) {
		t.Fatalf("retained evaluation: %s", res.Content)
	}
	res = call(t, r, tc, "run_code", map[string]any{"code": "throw new Error('expected')"})
	if !res.IsError {
		t.Fatal("exception wasn't surfaced")
	}
	res = call(t, r, tc, "run_code", map[string]any{"code": "retained"})
	if res.IsError || !strings.Contains(res.Content, `"value":42`) {
		t.Fatalf("exception discarded prior mutation: %s", res.Content)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := r.Execute(ctx, tc, "run_code", core.JSON(map[string]any{"code": "await new Promise(()=>{})"}))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel error %v", err)
	}
	res = call(t, r, tc, "run_code", map[string]any{"code": "typeof retained"})
	if res.IsError || !strings.Contains(res.Content, `"value":"undefined"`) {
		t.Fatalf("cancellation retained memory: %s", res.Content)
	}
	r.InvalidateSession(tc.SessionID)
}

func TestCredentialEnvironmentScrubbing(t *testing.T) {
	clean := processEnvironment([]string{"PATH=/bin", "HOME=/tmp", "OPENAI_API_KEY=hidden", "PASSWORD=hidden", "AWS_SECRET_ACCESS_KEY=hidden", "SESSION_TOKEN=hidden", "DSH_AUTH=hidden", "DSGO_HOME=hidden", "TERM=xterm"})
	if len(clean) != 3 {
		t.Fatalf("unexpected environment %v", clean)
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node unavailable")
	}
	if runtime.GOOS == "windows" {
		t.Skip("fd3 unsupported")
	}
	t.Setenv("DSGO_API_KEY", "never-inherit")
	t.Setenv("THIRD_PARTY_TOKEN", "never-inherit")
	t.Setenv("DSH_AUTH", "never-inherit")
	r := New()
	defer r.Close()
	tc := toolContext(t)
	tc.Permission = "full-access"
	res := call(t, r, tc, "run_code", map[string]any{"code": "return ['DSGO_API_KEY','THIRD_PARTY_TOKEN','DSH_AUTH'].map(k => process.env[k] ?? null);"})
	if res.IsError || !strings.Contains(res.Content, `"value":[null,null,null]`) {
		t.Fatalf("subprocess inherited harness credentials: %s", res.Content)
	}
	r.SetNodePath("/definitely/missing/dsgo-node")
	if _, err := r.Execute(context.Background(), tc, "run_code", core.JSON(map[string]any{"code": "return 1"})); err == nil {
		t.Fatal("configured NodePath was ignored")
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
			_, err := r.Execute(context.Background(), tc, "write_file", core.JSON(map[string]any{"path": "never-created", "content": "no"}))
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

func TestPTCNestedDispatcherAndAuditCannotBeBypassed(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node unavailable")
	}
	if runtime.GOOS == "windows" {
		t.Skip("fd3 unsupported")
	}
	for _, mode := range []string{"per-call", "persistent-in-memory"} {
		t.Run(mode, func(t *testing.T) {
			r := New()
			defer r.Close()
			r.SetPTCMode(mode)
			tc := toolContext(t)
			tc.Permission = "full-access"
			code := "return await tools.write_file({path:'blocked',content:'no'});"
			if mode == "persistent-in-memory" {
				code = "await tools.write_file({path:'blocked',content:'no'})"
			}
			dispatches := 0
			r.SetDispatcher(func(ctx context.Context, tc core.ToolContext, name string, input json.RawMessage) (core.ToolResult, error) {
				dispatches++
				return core.ToolResult{}, errors.New("outer policy denied nested call")
			})
			res := call(t, r, tc, "run_code", map[string]any{"code": code})
			if !res.IsError || !strings.Contains(res.Content, "outer policy denied") || dispatches != 1 {
				t.Fatalf("dispatcher bypass: %s (%d calls)", res.Content, dispatches)
			}
			r.SetDispatcher(func(ctx context.Context, tc core.ToolContext, name string, input json.RawMessage) (core.ToolResult, error) {
				dispatches++
				return r.Execute(ctx, tc, name, input)
			})
			tc.Emit = func(kind string, v any) error {
				if kind == "tool/subcall" {
					return errors.New("disk full")
				}
				return nil
			}
			_, err := r.Execute(context.Background(), tc, "run_code", core.JSON(map[string]any{"code": code}))
			if err == nil || !strings.Contains(err.Error(), "audit failed") {
				t.Fatalf("nested audit failure not surfaced: %v", err)
			}
			if dispatches != 1 {
				t.Fatal("side effect dispatched after failed audit")
			}
			if _, err := os.Stat(filepath.Join(tc.CWD, "blocked")); !os.IsNotExist(err) {
				t.Fatal("nested policy/audit bypass wrote file")
			}
		})
	}
}
