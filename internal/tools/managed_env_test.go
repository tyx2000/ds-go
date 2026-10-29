package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"dsgo/internal/core"
)

func environmentMap(env []string) map[string]string {
	out := map[string]string{}
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			out[name] = value
		}
	}
	return out
}

func TestManagedEnvOfficialSourceCorpus(t *testing.T) {
	data, err := os.ReadFile("testdata/managed-env-corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Cases []struct {
			Name, Kind, Home, Session, PTY string
			Parent, Want                   map[string]string
			Profile                        *struct{ Name, Dir string }
		}
	}
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	for _, test := range corpus.Cases {
		t.Run(test.Name, func(t *testing.T) {
			base := []string{}
			for key, value := range test.Parent {
				base = append(base, key+"="+value)
			}
			base = processEnvironment(base)
			tc := core.ToolContext{SessionID: test.Session, HarnessHome: test.Home}
			if test.Profile != nil {
				tc.ProfileName, tc.ProfileDir = test.Profile.Name, test.Profile.Dir
			}
			var err error
			switch test.Kind {
			case "fresh":
				base, err = freshShellEnvironment(base, tc)
			case "persistent":
				base = persistentShellEnvironment(base, test.Session, test.PTY)
			case "host":
				base = overlayEnvironment(base, map[string]string{"DSH_SESSION_ID": test.Session})
			case "generic":
			default:
				t.Fatal("unknown fixture kind")
			}
			if err != nil {
				t.Fatal(err)
			}
			facts := map[string]string{}
			for key, value := range environmentMap(base) {
				if strings.HasPrefix(strings.ToUpper(key), "DSH_") {
					facts[key] = value
				}
			}
			if !reflect.DeepEqual(facts, test.Want) {
				t.Fatalf("facts %+v want %+v", facts, test.Want)
			}
		})
	}
}

func TestManagedFreshShellFactsAndHomePrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DSH_HOME", filepath.Join(home, "ambient"))
	base := processEnvironment([]string{"PATH=/bin", "HOME=" + home, "DSH_HOME=stale", "DSH_SHELL=spoof", "dsh_stale_parent=stale", "DSGO_HOME=stale", "ORDINARY=keep", "KEYBOARD=removed", "api_token=removed"})
	before := append([]string(nil), base...)
	tc := core.ToolContext{SessionID: "session-a", CWD: filepath.Join(home, "unrelated-cwd"), HarnessHome: filepath.Join(home, "actual-data"), ProfileName: "web", ProfileDir: filepath.Join(home, "profiles", "web")}
	got, err := freshShellEnvironment(base, tc)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"PATH": "/bin", "HOME": home, "ORDINARY": "keep", "DSH_HOME": tc.HarnessHome, "DSH_SHELL": "1", "DSH_SESSION_ID": "session-a", "DSH_PROFILE": "web", "DSH_PROFILE_DIR": tc.ProfileDir}
	if !reflect.DeepEqual(environmentMap(got), want) {
		t.Fatalf("facts: %+v", environmentMap(got))
	}
	if !reflect.DeepEqual(base, before) {
		t.Fatal("environment merge mutated its caller")
	}
	tc = core.ToolContext{}
	got, err = freshShellEnvironment(base, tc)
	if err != nil {
		t.Fatal(err)
	}
	values := environmentMap(got)
	if values["DSH_HOME"] != filepath.Join(home, "ambient") || values["DSH_SHELL"] != "1" {
		t.Fatalf("ambient resolution: %+v", values)
	}
	for _, key := range []string{"DSH_SESSION_ID", "DSH_PROFILE", "DSH_PROFILE_DIR", "DSH_PTY_SESSION_ID"} {
		if _, ok := values[key]; ok {
			t.Errorf("non-owner fact %s injected", key)
		}
	}
	t.Setenv("DSH_HOME", " \t\r\n")
	resolved, err := managedHarnessHome("")
	if err != nil || resolved != filepath.Join(home, ".dsh") {
		t.Fatalf("default home %q %v", resolved, err)
	}
	for _, test := range []struct{ value, want string }{{"~", home}, {"~/child", filepath.Join(home, "child")}, {"~\\child", filepath.Join(home, "child")}} {
		resolved, err := managedHarnessHome(test.value)
		if err != nil || resolved != test.want {
			t.Fatalf("home %q = %q %v", test.value, resolved, err)
		}
	}
	if trimEnvironmentSpace("\u0085") != "\u0085" || trimEnvironmentSpace("\ufeff \t") != "" {
		t.Fatal("environment trim differs from JavaScript whitespace")
	}
}

func TestManagedShellNamespacesStaySeparateFromGenericAndHostProcesses(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DSH_STALE_PARENT", "drop")
	t.Setenv("dSh_Session_Id", "spoof")
	t.Setenv("DSGO_API_KEY", "drop")
	tc := core.ToolContext{SessionID: "owner", CWD: root, HarnessHome: filepath.Join(root, "state"), ProfileName: "web", ProfileDir: "/profiles/web"}
	sc, err := makeToolCommand(context.Background(), tc, "full-access", "unused-fixture-executable")
	if err != nil {
		t.Fatal(err)
	}
	defer sc.cleanup()
	for key := range environmentMap(sc.cmd.Env) {
		if strings.HasPrefix(strings.ToUpper(key), "DSH_") {
			t.Fatalf("generic process acquired shell fact %s", key)
		}
	}
	base := processEnvironment(os.Environ())
	managed := environmentMap(persistentShellEnvironment(base, tc.SessionID, "pty-7"))
	for key, want := range map[string]string{"DSH_SESSION_ID": "owner", "DSH_SHELL": "1", "DSH_PTY_SESSION_ID": "pty-7", "TERM": "dumb", "PAGER": "cat", "GIT_PAGER": "cat"} {
		if managed[key] != want {
			t.Errorf("persistent %s = %q", key, managed[key])
		}
	}
	for _, key := range []string{"DSH_HOME", "DSH_PROFILE", "DSH_PROFILE_DIR", "DSH_STALE_PARENT"} {
		if _, ok := managed[key]; ok {
			t.Errorf("fresh/ambient fact %s entered model PTY", key)
		}
	}
	// This is the Host API's explicit layer, not the model's PTY layer.
	host := environmentMap(overlayEnvironment(base, map[string]string{"DSH_SESSION_ID": "host-owner", "TERM": "xterm-256color"}))
	for _, key := range []string{"DSH_HOME", "DSH_PROFILE", "DSH_SHELL", "DSH_PTY_SESSION_ID"} {
		if _, ok := host[key]; ok {
			t.Errorf("model fact %s entered Host terminal", key)
		}
	}
}
