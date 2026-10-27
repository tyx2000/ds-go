package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dsgo/internal/state"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func jsonReply(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestCLIConversationToolResumeAndJSON(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "hello.txt"), []byte("workspace fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DSGO_API_KEY", "fixture-key")
	t.Setenv("DSGO_BASE_URL", "https://example.invalid/anthropic")
	original := http.DefaultTransport
	defer func() { http.DefaultTransport = original }()
	requests := 0
	http.DefaultTransport = roundTrip(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Path != "/anthropic/v1/messages" {
			t.Fatalf("unexpected endpoint %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "fixture-key" {
			t.Fatal("missing credential")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		switch requests {
		case 1:
			return jsonReply(`{"id":"m1","type":"message","content":[{"type":"tool_use","id":"call1","name":"read_file","input":{"path":"hello.txt"}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":4}}`), nil
		case 2:
			b, _ := json.Marshal(body["messages"])
			if !bytes.Contains(b, []byte("workspace fixture")) {
				t.Fatal("tool output was not replayed")
			}
			return jsonReply(`{"id":"m2","type":"message","content":[{"type":"text","text":"File read successfully."}],"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":5}}`), nil
		default:
			b, _ := json.Marshal(body["messages"])
			if !bytes.Contains(b, []byte("File read successfully.")) {
				t.Fatal("history lost across resume")
			}
			return jsonReply(`{"id":"m3","type":"message","content":[{"type":"text","text":"Resumed."}],"stop_reason":"end_turn"}`), nil
		}
	})
	var out, errOut bytes.Buffer
	code := Main([]string{"--home", home, "run", "--cwd", cwd, "--json", "read the fixture"}, strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	scan := bufio.NewScanner(&out)
	lines := 0
	for scan.Scan() {
		if !json.Valid(scan.Bytes()) {
			t.Fatalf("not NDJSON: %s", scan.Text())
		}
		lines++
	}
	if lines < 5 {
		t.Fatal("event output missing")
	}
	s, _ := state.Open(home)
	sessions, err := s.List(false)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions %v %v", sessions, err)
	}
	out.Reset()
	errOut.Reset()
	code = Main([]string{"--home", home, "run", "--resume", sessions[0].ID, "continue"}, strings.NewReader(""), &out, &errOut)
	if code != 0 || !strings.Contains(out.String(), "Resumed.") {
		t.Fatalf("resume exit=%d out=%s err=%s", code, out.String(), errOut.String())
	}
	if requests != 3 {
		t.Fatalf("requests=%d", requests)
	}
}

func TestConfigCredentialIsMaskedAndEnvironmentNotPersisted(t *testing.T) {
	home := t.TempDir()
	var out, errOut bytes.Buffer
	if code := Main([]string{"--home", home, "config", "set", "api_key", "--stdin"}, strings.NewReader("private-fixture\n"), &out, &errOut); code != 0 {
		t.Fatal(errOut.String())
	}
	out.Reset()
	if code := Main([]string{"--home", home, "config", "show"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatal(errOut.String())
	}
	if strings.Contains(out.String(), "private-fixture") {
		t.Fatal("credential leaked")
	}
	t.Setenv("DSGO_API_KEY", "environment-fixture")
	out.Reset()
	if code := Main([]string{"--home", home, "config", "set", "thinking", "max"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatal(errOut.String())
	}
	cfg, err := state.LoadConfig(home)
	if err != nil || cfg.APIKey != "private-fixture" {
		t.Fatalf("environment credential persisted: %v", err)
	}
}

func TestCancelledPromptCannotAuthorizeNextPrompt(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &App{in: bufio.NewReader(reader), out: io.Discard, ctx: ctx}
	firstCtx, firstCancel := context.WithCancel(ctx)
	first := make(chan error, 1)
	go func() { _, err := a.readLine(firstCtx); first <- err }()
	time.Sleep(10 * time.Millisecond)
	firstCancel()
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled question blocked turn shutdown")
	}
	second := make(chan string, 1)
	go func() { s, _ := a.readLine(ctx); second <- s }()
	_, _ = io.WriteString(writer, "yes\n")
	select {
	case <-second:
		t.Fatal("late answer reused for next question")
	case <-time.After(20 * time.Millisecond):
	}
	_, _ = io.WriteString(writer, "no\n")
	select {
	case s := <-second:
		if s != "no" {
			t.Fatal(s)
		}
	case <-time.After(time.Second):
		t.Fatal("new question did not receive its answer")
	}
}

func TestTerminalControlOutputIsRemoved(t *testing.T) {
	got := clean("hello\x1b]52;c;token\a\n\x00\u009bworld")
	if strings.ContainsAny(got, "\x1b\x00\a\u009b") {
		t.Fatal("terminal control was retained")
	}
}

func TestCLIUsesSelectedOfficialProviderProtocolAndItsCredential(t *testing.T) {
	home := t.TempDir()
	cfg := state.DefaultConfig()
	cfg.SelectedProvider = "configured"
	cfg.Model = "fixture-model"
	cfg.Thinking = "off"
	cfg.ProviderProfiles = map[string]json.RawMessage{"configured": json.RawMessage(`{"api":"openai-completions","base_url":"https://provider.invalid/v9","api_key_env":"DSGO_FIXTURE_PROVIDER_CREDENTIAL"}`)}
	if err := state.SaveConfig(home, cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DSGO_FIXTURE_PROVIDER_CREDENTIAL", "fixture-provider-secret")
	original := http.DefaultTransport
	defer func() { http.DefaultTransport = original }()
	called := false
	http.DefaultTransport = roundTrip(func(r *http.Request) (*http.Response, error) {
		called = true
		if r.URL.Host != "provider.invalid" || r.URL.Path != "/v9/chat/completions" {
			t.Fatalf("incorrect selected route: %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer fixture-provider-secret" || r.Header.Get("x-api-key") != "" {
			t.Fatal("provider credential confused with the DeepSeek key")
		}
		return jsonReply(`{"id":"chat1","choices":[{"index":0,"message":{"role":"assistant","content":"Provider fixture complete"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`), nil
	})
	var out, stderr bytes.Buffer
	if code := Main([]string{"--home", home, "run", "--cwd", t.TempDir(), "fixture"}, strings.NewReader(""), &out, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !called || !strings.Contains(out.String(), "Provider fixture complete") || strings.Contains(out.String(), "fixture-provider-secret") {
		t.Fatalf("route output was missing or leaked the credential: %s", out.String())
	}
}
