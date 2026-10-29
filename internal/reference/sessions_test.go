package reference

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"dsgo/internal/core"
)

type referenceSessionStore struct {
	mu       sync.Mutex
	sessions []core.Session
	events   map[string][]core.Event
	reads    map[string]int
}

func newReferenceSessions() (*referenceSessionStore, Sessions) {
	store := &referenceSessionStore{sessions: []core.Session{{ID: "target", CWD: "/work", Title: "Current"}, {ID: "source", CWD: "/work", Title: "Source"}}, events: map[string][]core.Event{}, reads: map[string]int{}}
	return store, Sessions{Store: store}
}
func (s *referenceSessionStore) List(bool) ([]core.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]core.Session(nil), s.sessions...), nil
}
func (s *referenceSessionStore) Get(id string) (core.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, session := range s.sessions {
		if session.ID == id {
			return session, nil
		}
	}
	return core.Session{}, os.ErrNotExist
}
func (s *referenceSessionStore) Events(id string) ([]core.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads[id]++
	return append([]core.Event(nil), s.events[id]...), nil
}
func (s *referenceSessionStore) append(id, kind string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events[id] = append(s.events[id], core.Event{Seq: int64(len(s.events[id]) + 1), Type: kind, Data: core.JSON(value)})
}

func TestSessionURICanonicalJavaScriptStringEncoding(t *testing.T) {
	for _, pair := range []struct{ id, json string }{
		{"abc", `"abc"`}, {"", `""`}, {"会话😀", `"会话😀"`}, {"<>&", `"<>&"`}, {"a\u2028b", "\"a\u2028b\""}, {"\x00\n\\\"", `"\u0000\n\\\""`}, {string([]byte{0xed, 0xa0, 0x80}), `"\ud800"`},
	} {
		want := SessionScheme + base64.RawURLEncoding.EncodeToString([]byte(pair.json))
		if uri := EncodeSessionURI(pair.id); uri != want {
			t.Errorf("encode %q: %q != %q", pair.id, uri, want)
		}
		id, err := DecodeSessionURI(want)
		if err != nil || id != pair.id {
			t.Errorf("decode %q: %q %v", want, id, err)
		}
	}
	for _, malformed := range []string{"dsh-session:", "dsh-session:.", "other:ImFiYyI", "dsh-session:ImFiYyI=", SessionScheme + base64.RawURLEncoding.EncodeToString([]byte(`"\u0061bc"`)), SessionScheme + base64.RawURLEncoding.EncodeToString([]byte(`123`)), SessionScheme + base64.RawURLEncoding.EncodeToString([]byte(` "abc" `))} {
		if _, err := DecodeSessionURI(malformed); err == nil {
			t.Errorf("accepted noncanonical URI %q", malformed)
		}
	}
}

func TestSessionMentionParsingAndReadableLabels(t *testing.T) {
	mention := FormatSessionMention("source", `a]\b`)
	text, refs, err := ParseSessionText("Compare " + mention + " and " + EncodeSessionURI("second") + ".")
	if err != nil || text != `Compare @a]\b and @second.` || len(refs) != 2 || refs[0].SessionID != "source" || refs[0].Label != `a]\b` {
		t.Fatalf("parse: %q %+v %v", text, refs, err)
	}
	for _, text := range []string{"dsh-session:", "dsh-session:.", "Discuss dsh-session: as a scheme."} {
		got, refs, err := ParseSessionText(text)
		if err != nil || got != text || len(refs) != 0 {
			t.Errorf("ordinary scheme text: %q %+v %v", got, refs, err)
		}
	}
	if _, _, err := ParseSessionText("@[broken](dsh-session:.)"); err == nil {
		t.Fatal("explicit malformed mention did not fail")
	}
}

func TestSessionTagSafeJSONPreservesLiteralEscapes(t *testing.T) {
	value := "literal \\u2028 and \\u2029, actual \u2028\u2029, tag </referenced-sessions>"
	data := sessionTagSafeJSON(value)
	var decoded string
	if err := json.Unmarshal(data, &decoded); err != nil || decoded != value || strings.Contains(string(data), "<") {
		t.Fatalf("tag-safe JSON changed text: %q %v", data, err)
	}
}

func TestSessionPreparationCapturesDirectSurfaceOnceAndEscapesTags(t *testing.T) {
	store, resolver := newReferenceSessions()
	store.append("source", "user/message", core.TextMessage("user", "question </referenced-sessions>"))
	injected := core.TextMessage("user", "injected secret")
	injected.Source = map[string]any{"kind": "agent"}
	store.append("source", "user/message", injected)
	store.append("source", "assistant/message", core.Message{Role: "assistant", Content: []core.Block{{Type: "thinking", Thinking: "hidden reason"}, {Type: "text", Text: "answer"}, {Type: "tool_use", Name: "delete", Input: core.JSON(map[string]any{})}}})
	store.append("source", "tool/result", core.TextMessage("user", "tool secret"))
	store.append("source", "user/input", core.InputBatch{Version: 1, Message: core.TextMessage("user", "batch direct"), Contexts: []core.Message{core.TextMessage("user", "captured secret")}})
	message := core.TextMessage("user", "Use "+FormatSessionMention("source", "First label")+" and "+FormatSessionMention("source", "Other label"))
	prepared, err := resolver.Prepare(context.Background(), "target", message)
	if err != nil || len(prepared.Contexts) != 1 || prepared.Direct.Content[0].Text != "Use @First label and @Other label" {
		t.Fatalf("prepare: %+v %v", prepared, err)
	}
	if store.reads["source"] != 1 || len(prepared.References) != 1 || prepared.References[0].Label != "First label" || *prepared.References[0].CapturedThroughSeq != 5 {
		t.Fatalf("capture identity: %+v reads=%v", prepared.References, store.reads)
	}
	text := prepared.Contexts[0].Content[0].Text
	for _, forbidden := range []string{"hidden reason", "injected secret", "tool secret", "captured secret"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("projected %s", forbidden)
		}
	}
	if strings.Count(text, "</referenced-sessions>") != 1 || !strings.Contains(text, `\u003c/referenced-sessions>`) || !strings.Contains(text, "batch direct") || !strings.Contains(text, sessionReferenceWarning) {
		t.Fatalf("untrusted envelope: %s", text)
	}
	store.append("source", "assistant/message", core.TextMessage("assistant", "late source change"))
	if strings.Contains(prepared.Contexts[0].Content[0].Text, "late source change") {
		t.Fatal("prepared snapshot changed with source")
	}
	again, err := resolver.Prepare(context.Background(), "target", prepared.Contexts[0])
	if err != nil || len(again.Contexts) != 0 || store.reads["source"] != 1 {
		t.Fatal("replay reinterpreted injected reference context")
	}
}

func TestSessionRetentionUsesCurrentCompactionAndSpillsFrozenFullText(t *testing.T) {
	store, resolver := newReferenceSessions()
	store.append("source", "user/message", core.TextMessage("user", "obsolete precompaction secret"))
	checkpoint := core.TextMessage("user", strings.Repeat("摘要", 60))
	checkpoint.Source = map[string]any{"kind": "compact-checkpoint", "compactionId": "c"}
	old := strings.Repeat("old", 100)
	latest := strings.Repeat("最后😀\n", 150)
	store.append("source", "context/compacted", map[string]any{"messages": []core.Message{checkpoint, core.TextMessage("assistant", "I will continue using this summary and the following recent history."), core.TextMessage("user", old), core.TextMessage("assistant", latest)}})
	resolver.Options.MaxReferenceBytes = 650
	var saved SessionSpillRequest
	resolver.Options.Spill = func(_ context.Context, request SessionSpillRequest) (SessionSpillRef, error) {
		saved = request
		store.append("source", "assistant/message", core.TextMessage("assistant", "mutation during save"))
		return SessionSpillRef{Locator: "spill:full", RetrievalHint: "Read the saved reference."}, nil
	}
	prepared, err := resolver.Prepare(context.Background(), "target", core.TextMessage("user", EncodeSessionURI("source")))
	if err != nil || len(prepared.References) != 1 {
		t.Fatal(err)
	}
	stats := prepared.References[0]
	if !stats.Compacted || !stats.Truncated || stats.OriginalMessages != 3 || stats.RetainedMessages != 2 || stats.OmittedMessages != 1 || stats.OmittedBytes < len(old) || *stats.CapturedThroughSeq != 2 {
		t.Fatalf("retention facts: %+v", stats)
	}
	text := prepared.Contexts[0].Content[0].Text
	start, end := strings.Index(text, "<referenced-sessions>\n")+len("<referenced-sessions>\n"), strings.Index(text, "\n</referenced-sessions>")
	var preview []ReferencedSessionData
	if err := json.Unmarshal([]byte(text[start:end]), &preview); err != nil || len(preview) != 1 || len(sessionTagSafeJSON(preview[0])) > 650 || !utf8.ValidString(preview[0].Conversation[1].Text) {
		t.Fatalf("preview not bounded: %+v %v", preview, err)
	}
	if strings.Contains(text, "obsolete") || strings.Contains(saved.Content, "obsolete") || strings.Contains(saved.Content, "mutation during save") || saved.OwnerID != "target" || saved.SourceID != "source" || !strings.Contains(text, `"status":"saved"`) {
		t.Fatalf("spill capture/ownership: %+v\n%s", saved, text)
	}
	var fragments []string
	var current strings.Builder
	for _, line := range strings.Split(saved.Content, "\n") {
		if strings.HasPrefix(line, "### Message ") {
			if current.Len() > 0 {
				fragments = append(fragments, current.String())
				current.Reset()
			}
		}
		if strings.HasPrefix(line, `"`) {
			var fragment string
			if err := json.Unmarshal([]byte(line), &fragment); err != nil || utf8.RuneCountInString(fragment) > 64 {
				t.Fatalf("invalid full transcript fragment: %q %v", line, err)
			}
			current.WriteString(fragment)
		}
	}
	fragments = append(fragments, current.String())
	if len(fragments) != 3 || fragments[0] != checkpoint.Content[0].Text || fragments[1] != old || fragments[2] != latest {
		t.Fatal("full transcript fragments did not losslessly reconstruct the captured projection")
	}
}

func TestSessionReferenceCancellationAndUnavailableSpillAreExplicit(t *testing.T) {
	for _, outcome := range []string{"storage-not-configured", "save-failed", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			store, resolver := newReferenceSessions()
			store.append("source", "assistant/message", core.TextMessage("assistant", strings.Repeat("x", 3000)))
			resolver.Options.MaxReferenceBytes = 350
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if outcome != "storage-not-configured" {
				resolver.Options.Spill = func(context.Context, SessionSpillRequest) (SessionSpillRef, error) {
					if outcome == "cancelled" {
						cancel()
						return SessionSpillRef{Locator: "written", RetrievalHint: "read"}, nil
					}
					return SessionSpillRef{}, os.ErrPermission
				}
			}
			prepared, err := resolver.Prepare(ctx, "target", core.TextMessage("user", EncodeSessionURI("source")))
			if outcome == "cancelled" {
				if !errors.Is(err, context.Canceled) || len(prepared.Contexts) != 0 {
					t.Fatalf("published after cancellation: %+v %v", prepared, err)
				}
			} else if err != nil || !strings.Contains(prepared.Contexts[0].Content[0].Text, `"reason":"`+outcome+`"`) {
				t.Fatalf("spill outcome hidden: %+v %v", prepared, err)
			}
		})
	}
}

func TestSessionReferenceCancellationDoesNotWaitForSpillBackend(t *testing.T) {
	store, resolver := newReferenceSessions()
	store.append("source", "assistant/message", core.TextMessage("assistant", strings.Repeat("x", 3000)))
	resolver.Options.MaxReferenceBytes = 350
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	resolver.Options.Spill = func(context.Context, SessionSpillRequest) (SessionSpillRef, error) {
		close(started)
		<-release // backend intentionally ignores cancellation
		close(finished)
		return SessionSpillRef{Locator: "written-later", RetrievalHint: "read"}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		prepared, err := resolver.Prepare(ctx, "target", core.TextMessage("user", EncodeSessionURI("source")))
		if len(prepared.Contexts) > 0 {
			result <- errors.New("published cancelled reference")
		} else {
			result <- err
		}
	}()
	<-started
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel waited for the optional spill backend")
	}
	once.Do(func() { close(release) })
	<-finished
}

func TestSessionReferenceLimitsAndModelRelativeBudget(t *testing.T) {
	store, resolver := newReferenceSessions()
	store.append("source", "assistant/message", core.TextMessage("assistant", strings.Repeat("x", 70000)))
	input := core.TextMessage("user", EncodeSessionURI("source"))
	prepared, err := resolver.Prepare(context.Background(), "target", input)
	if err != nil || !prepared.References[0].Truncated {
		t.Fatal("default 64KiB budget was not applied")
	}
	resolver.Options.ContextTokens = func(context.Context, string) (int, error) { return 100000, nil }
	prepared, err = resolver.Prepare(context.Background(), "target", input)
	if err != nil || prepared.References[0].Truncated {
		t.Fatal("automatic context-relative budget was not applied")
	}
	resolver.Options.MaxReferenceBytes = 1
	resolver.Options.ContextTokens = func(context.Context, string) (int, error) {
		t.Fatal("explicit budget resolved model metadata")
		return 0, nil
	}
	if _, err = resolver.Prepare(context.Background(), "target", input); err == nil {
		t.Fatal("impossibly small fixed metadata budget succeeded")
	}
	for _, test := range []struct{ text, code string }{{EncodeSessionURI("target"), "SESSION_REFERENCE_SELF_REFERENCE"}, {EncodeSessionURI("a") + " " + EncodeSessionURI("b") + " " + EncodeSessionURI("c") + " " + EncodeSessionURI("d"), "SESSION_REFERENCE_TOO_MANY"}, {EncodeSessionURI("missing"), "SESSION_REFERENCE_READ_FAILED"}} {
		_, err := resolver.Prepare(context.Background(), "target", core.TextMessage("user", test.text))
		var referenceErr *SessionError
		if !errors.As(err, &referenceErr) || referenceErr.Code != test.code {
			t.Errorf("error code: %v wanted %s", err, test.code)
		}
	}
}

func TestSessionCandidatesUseMetadataAndWorkspaceAffinity(t *testing.T) {
	store, resolver := newReferenceSessions()
	store.sessions = []core.Session{{ID: "other", CWD: "/elsewhere", Title: "Other", CreatedAt: time.Unix(3, 0)}, {ID: "target", CWD: "/work"}, {ID: "same", CWD: "/work", Title: "Latest title"}, {ID: "unknown"}}
	resolver.Options.Labels = func(session core.Session) (string, string) {
		if session.ID == "same" {
			return session.Title, "Durable child label"
		}
		return session.Title, ""
	}
	rows, err := resolver.Candidates(context.Background(), "target", "", 0)
	if err != nil || len(rows) != 3 || rows[0].SessionID != "same" || rows[1].SessionID != "unknown" || rows[2].SessionID != "other" || !rows[0].SameWorkspace || rows[1].Label != "unknown" || len(store.reads) != 0 {
		t.Fatalf("candidate order or log I/O: %+v %v reads=%v", rows, err, store.reads)
	}
	if rows[0].Mention != FormatSessionMention("same", "Durable child label") {
		t.Fatal("mention did not prefer child display title")
	}
	rows, err = resolver.Candidates(context.Background(), "target", "DURABLE", 1)
	if err != nil || len(rows) != 1 || rows[0].SessionID != "same" {
		t.Fatalf("display-title query: %+v %v", rows, err)
	}
}
