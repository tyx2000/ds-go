package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"dsgo/internal/core"
)

type sandboxCommand struct {
	cmd     *exec.Cmd
	cleanup func()
	backend string
	mode    string
}

func requestedMode(ctx context.Context, tc core.ToolContext, name string, input json.RawMessage, requested string) (string, error) {
	standing := tc.Permission
	if standing == "" {
		standing = "workspace-write"
	}
	if requested == "" {
		requested = standing
	}
	rank := map[string]int{"read-only": 0, "workspace-write": 1, "full-access": 2}
	s, ok := rank[standing]
	if !ok {
		return "", errors.New("unknown standing sandbox mode")
	}
	n, ok := rank[requested]
	if !ok {
		return "", errors.New("unsupported sandbox mode")
	}
	if n > s {
		if err := ask(ctx, tc, name, "Allow this one process with "+requested+" filesystem access?", input); err != nil {
			return "", err
		}
	}
	return requested, nil
}

func workspaceRoot(tc core.ToolContext) string {
	if tc.WorkspaceRoot != "" {
		return tc.WorkspaceRoot
	}
	return tc.CWD
}

func makeToolCommand(ctx context.Context, tc core.ToolContext, mode, executable string, args ...string) (sandboxCommand, error) {
	return makeCommandAt(ctx, tc.CWD, workspaceRoot(tc), mode, executable, args...)
}

func makeCommandAt(ctx context.Context, cwd, workspace, mode, executable string, args ...string) (sandboxCommand, error) {
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return sandboxCommand{}, err
	}
	st, err := os.Stat(resolved)
	if err != nil || !st.IsDir() {
		return sandboxCommand{}, errors.New("cwd is not a directory")
	}
	writable, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return sandboxCommand{}, err
	}
	rootInfo, err := os.Stat(writable)
	if err != nil || !rootInfo.IsDir() {
		return sandboxCommand{}, errors.New("workspace root is not a directory")
	}
	tmp, err := os.MkdirTemp("", "dsgo-process-")
	if err != nil {
		return sandboxCommand{}, err
	}
	tmp, err = filepath.EvalSymlinks(tmp)
	if err != nil {
		return sandboxCommand{}, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	backend := "none"
	command := executable
	argv := append([]string(nil), args...)
	if mode != "full-access" {
		switch runtime.GOOS {
		case "darwin":
			path, e := exec.LookPath("sandbox-exec")
			if e != nil {
				cleanup()
				return sandboxCommand{}, errors.New("restricted mode unavailable: macOS sandbox-exec was not found")
			}
			// This is filesystem enforcement. Network and process creation retain normal host access.
			profile := "(version 1)\n(allow default)\n(deny file-write*)\n"
			profile += "(allow file-write* (subpath " + seatbeltQuote(tmp) + "))\n"
			profile += "(allow file-write* (literal \"/dev/null\") (literal \"/dev/tty\") (regex #\"^/dev/fd/[0-9]+$\"))\n"
			if mode == "workspace-write" {
				profile += "(allow file-write* (subpath " + seatbeltQuote(writable) + "))\n"
			}
			argv = append([]string{"-p", profile, executable}, args...)
			command = path
			backend = "macos-seatbelt"
		case "linux":
			path, e := exec.LookPath("bwrap")
			if e != nil {
				cleanup()
				return sandboxCommand{}, errors.New("restricted mode unavailable: install bubblewrap, or deliberately select full-access")
			}
			argv = []string{"--die-with-parent", "--new-session", "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--bind", tmp, tmp, "--chdir", resolved}
			if mode == "workspace-write" {
				argv = append(argv, "--bind", writable, writable)
			}
			argv = append(argv, "--", executable)
			argv = append(argv, args...)
			command = path
			backend = "linux-bubblewrap"
		default:
			cleanup()
			return sandboxCommand{}, fmt.Errorf("restricted mode unavailable on %s; no weaker fallback is used", runtime.GOOS)
		}
	}
	cmd := exec.CommandContext(ctx, command, argv...)
	cmd.Dir = resolved
	cmd.Env = processEnvironment(os.Environ())
	cmd.Env = append(cmd.Env, "TMPDIR="+tmp, "TMP="+tmp, "TEMP="+tmp)
	configureProcess(cmd)
	cmd.WaitDelay = 2 * time.Second
	return sandboxCommand{cmd: cmd, cleanup: cleanup, backend: backend, mode: mode}, nil
}
func filteredEnv(env []string, names ...string) []string {
	out := []string{}
	for _, e := range env {
		keep := true
		for _, n := range names {
			if strings.HasPrefix(e, n+"=") {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, e)
		}
	}
	return out
}

// Credentials supplied to the harness are not inherited by model-written processes.
func processEnvironment(env []string) []string {
	clean := filteredEnv(env, "TMPDIR", "TMP", "TEMP")
	out := make([]string, 0, len(clean))
	for _, e := range clean {
		name, _, _ := strings.Cut(e, "=")
		upper := strings.ToUpper(name)
		sensitive := strings.HasPrefix(upper, "DSH_") || strings.HasPrefix(upper, "DSGO_")
		for _, part := range []string{"KEY", "PASSWORD", "SECRET", "TOKEN"} {
			if strings.Contains(upper, part) {
				sensitive = true
				break
			}
		}
		if !sensitive {
			out = append(out, e)
		}
	}
	return out
}

func seatbeltQuote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

type job struct {
	standard                          *standardBashState
	mu                                sync.Mutex
	id, owner, command, backend, mode string
	pid                               int
	cancel                            context.CancelFunc
	done                              chan struct{}
	started, finished                 time.Time
	exitCode                          int
	err                               string
	notify                            chan struct{}
	waiters                           int           // live waits a settlement releases instead of notifying
	killedByModel                     bool          // job_kill's result is the model's delivery
	noticed                           chan struct{} // closed once a completion notice is handled
	outputLimitBytes                  int           // immutable producer cap; zero leaves model output unbounded
}
