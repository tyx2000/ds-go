package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"dsgo/internal/core"
)

func testStore(t *testing.T) (*Store, core.Session) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.Create(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	return s, session
}

func appendMessage(t *testing.T, s *Store, id, typ, text string) core.Event {
	t.Helper()
	role := "user"
	if typ == "assistant/message" {
		role = "assistant"
	}
	ev, err := s.Append(id, "turn", typ, core.TextMessage(role, text))
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestMessagesRetainThinkingToolsAndCompaction(t *testing.T) {
	s, session := testStore(t)
	appendMessage(t, s, session.ID, "user/message", "original")
	_, err := s.Append(session.ID, "turn", "assistant/message", core.Message{Role: "assistant", Content: []core.Block{{Type: "thinking", Thinking: "reason", Signature: "sig"}, {Type: "tool_use", ID: "tool1", Name: "read_file", Input: json.RawMessage(`{"path":"x"}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Append(session.ID, "turn", "tool/result", core.Message{Role: "user", Content: []core.Block{{Type: "tool_result", ToolUseID: "tool1", Content: "value"}}})
	if err != nil {
		t.Fatal(err)
	}
	appendMessage(t, s, session.ID, "assistant/attempt", "failed response must not enter model context")
	msgs, err := s.Messages(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 || msgs[1].Content[0].Signature != "sig" || msgs[2].Content[0].ToolUseID != "tool1" {
		t.Fatalf("lost replay content: %+v", msgs)
	}
	_, err = s.Append(session.ID, "", "context/compacted", map[string]any{"messages": []core.Message{core.TextMessage("user", "summary")}})
	if err != nil {
		t.Fatal(err)
	}
	appendMessage(t, s, session.ID, "user/message", "after")
	msgs, err = s.Messages(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Content[0].Text != "summary" || msgs[1].Content[0].Text != "after" {
		t.Fatalf("bad compacted context: %+v", msgs)
	}
	events, err := s.Events(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 6 {
		t.Fatalf("compaction removed history: %d", len(events))
	}
}

func TestTailRecoveryPreservesCompleteEventWithoutNewline(t *testing.T) {
	s, session := testStore(t)
	appendMessage(t, s, session.ID, "user/message", "first")
	appendMessage(t, s, session.ID, "assistant/message", "second")
	path := filepath.Join(s.Root(), "sessions", session.ID, "events.jsonl")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.TrimSuffix(original, []byte{'\n'}), 0600); err != nil {
		t.Fatal(err)
	}
	appendMessage(t, s, session.ID, "user/message", "third")
	events, err := s.Events(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[1].Type != "assistant/message" {
		t.Fatalf("lost complete tail: %+v", events)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`{"seq":4,"id":"incomplete`)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if events, err := s.Events(session.ID); err != nil || len(events) != 3 {
		t.Fatalf("read valid prefix: %d %v", len(events), err)
	}
	appendMessage(t, s, session.ID, "assistant/message", "fourth")
	events, err = s.Events(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || events[3].Seq != 4 {
		t.Fatalf("bad repair: %+v", events)
	}
}

func TestMiddleCorruptionIsNotSilentlyTruncated(t *testing.T) {
	s, session := testStore(t)
	appendMessage(t, s, session.ID, "user/message", "first")
	path := filepath.Join(s.Root(), "sessions", session.ID, "events.jsonl")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte("{broken}\n"), original...)
	if err := os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Events(session.ID); err == nil {
		t.Fatal("accepted corrupt committed record")
	}
	if _, err := s.Append(session.ID, "", "test", map[string]string{}); err == nil {
		t.Fatal("appended through corruption")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, corrupt) {
		t.Fatal("modified corrupted log")
	}
}

func TestForkIsolationAndExport(t *testing.T) {
	s, session := testStore(t)
	appendMessage(t, s, session.ID, "user/message", "one")
	appendMessage(t, s, session.ID, "assistant/message", "two")
	appendMessage(t, s, session.ID, "user/message", "three")
	child, err := s.Fork(session.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if child.ParentID != session.ID {
		t.Fatal("missing ancestry")
	}
	appendMessage(t, s, child.ID, "user/message", "child only")
	parentMsgs, err := s.Messages(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	childMsgs, err := s.Messages(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(parentMsgs) != 3 || parentMsgs[2].Content[0].Text != "three" || len(childMsgs) != 3 || childMsgs[2].Content[0].Text != "child only" {
		t.Fatal("fork affected parent or wrong cut")
	}
	for _, format := range []string{"json", "jsonl", "markdown"} {
		b, err := s.Export(child.ID, format)
		if err != nil || !bytes.Contains(b, []byte("child only")) {
			t.Fatalf("export %s: %s %v", format, b, err)
		}
	}
	if _, err := s.Fork(session.ID, 10); err == nil {
		t.Fatal("accepted nonexistent fork point")
	}
}

func TestSessionLockDoesNotBlockAppendAndReleases(t *testing.T) {
	s, session := testStore(t)
	s2, err := Open(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	release, err := s.AcquireSession(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.AcquireSession(context.Background(), session.ID); !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent turn accepted: %v", err)
	}
	appendMessage(t, s, session.ID, "user/message", "append while turn held")
	if err := s2.Delete(session.ID); !errors.Is(err, ErrBusy) {
		t.Fatalf("deleted live session: %v", err)
	}
	release()
	release()
	release2, err := s2.AcquireSession(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	release2()
	if err := s2.Delete(session.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRejectTraversalAndSymlink(t *testing.T) {
	s, session := testStore(t)
	for _, id := range []string{"../config.json", "..", session.ID + "/../..", "UPPERCASE", ""} {
		if _, err := s.Get(id); err == nil {
			t.Errorf("accepted ID %q", id)
		}
		if _, err := s.Append(id, "", "test", nil); err == nil {
			t.Errorf("accepted append ID %q", id)
		}
	}
	target := filepath.Join(t.TempDir(), "untouched")
	if err := os.WriteFile(target, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(s.Root(), "sessions", session.ID, "meta.json")
	if err := os.Remove(meta); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, meta); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(session.ID); err == nil {
		t.Fatal("followed state symlink")
	}
	b, err := os.ReadFile(target)
	if err != nil || string(b) != "private" {
		t.Fatal("modified symlink target")
	}
}

func TestConcurrentStoreInstances(t *testing.T) {
	s, session := testStore(t)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			other, err := Open(s.Root())
			if err != nil {
				errs <- err
				return
			}
			for i := 0; i < 15; i++ {
				if _, err := other.Append(session.ID, "", "test", map[string]int{"i": i}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	events, err := s.Events(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 60 {
		t.Fatalf("concurrent records: %d", len(events))
	}
}

func TestProcessAppend(t *testing.T) {
	if root := os.Getenv("DSGO_STATE_TEST_ROOT"); root != "" {
		s, err := Open(root)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 10; i++ {
			if _, err := s.Append(os.Getenv("DSGO_STATE_TEST_ID"), "", "test", map[string]int{"i": i}); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	s, session := testStore(t)
	var commands []*exec.Cmd
	for i := 0; i < 3; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestProcessAppend$")
		cmd.Env = append(os.Environ(), "DSGO_STATE_TEST_ROOT="+s.Root(), "DSGO_STATE_TEST_ID="+session.ID)
		commands = append(commands, cmd)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
	}
	for _, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	events, err := s.Events(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 30 {
		t.Fatalf("cross-process record count: %d", len(events))
	}
}

func TestPermissionsAndConfig(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	c := DefaultConfig()
	c.APIKey = "secret"
	c.Account = json.RawMessage(`{"token":"account-secret","device":"identity"}`)
	c.ProviderProfiles["provider"] = json.RawMessage(`{"apiKey":"profile-secret","model":"test"}`)
	c.MCPServers["server"] = MCPServer{Command: "example", Env: map[string]string{"TOKEN": "password"}}
	if err := SaveConfig(root, c); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.APIKey != "secret" {
		t.Fatal("key lost")
	}
	if loaded.Masked().APIKey == "secret" || loaded.Masked().MCPServers["server"].Env["TOKEN"] == "password" {
		t.Fatal("secret exposed in masked config")
	}
	if bytes.Contains(loaded.Masked().Account, []byte("account-secret")) || !bytes.Contains(loaded.Account, []byte("account-secret")) {
		t.Fatal("account masking leaked or mutated credential")
	}
	if bytes.Contains(loaded.Masked().ProviderProfiles["provider"], []byte("profile-secret")) || !bytes.Contains(loaded.ProviderProfiles["provider"], []byte("profile-secret")) {
		t.Fatal("profile masking leaked or mutated credential")
	}
	if loaded.MCPServers["server"].Env["TOKEN"] != "password" {
		t.Fatal("Masked mutated original")
	}
	for _, entry := range []struct {
		path string
		mode os.FileMode
	}{{root, 0700}, {filepath.Join(root, "sessions"), 0700}, {filepath.Join(root, "config.json"), 0600}} {
		info, err := os.Stat(entry.path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != entry.mode {
			t.Errorf("%s: mode %o", entry.path, info.Mode().Perm())
		}
	}
	t.Setenv("DEEPSEEK_API_KEY", "env-secret")
	if loaded.WithEnvironment().APIKey != "env-secret" {
		t.Fatal("environment not applied")
	}
	loaded, err = LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.APIKey != "secret" {
		t.Fatal("environment leaked into persisted config")
	}
	c.Thinking = "invalid"
	if err := SaveConfig(root, c); err == nil {
		t.Fatal("invalid thinking accepted")
	}
	b, err := os.ReadFile(filepath.Join(root, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("invalid")) {
		t.Fatal("failed validation changed config")
	}
}
