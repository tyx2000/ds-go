package attachments

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"dsgo/internal/core"
)

type transportFunc func(context.Context, core.Request, func(core.Delta)) (core.Response, error)

func (f transportFunc) Stream(ctx context.Context, r core.Request, emit func(core.Delta)) (core.Response, error) {
	return f(ctx, r, emit)
}
func TestImageReferencesStaySmallAndResolveOnlyAtTransport(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("image fixture")
	ref, err := s.Put(data, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	refAgain, err := s.Put(data, "image/png")
	if err != nil || refAgain["id"] != ref["id"] {
		t.Fatalf("not content addressed: %v", err)
	}
	message := core.Message{Role: "user", Content: []core.Block{{Type: "image", Source: ref}}}
	encoded, _ := json.Marshal(message)
	if bytes.Contains(encoded, []byte(base64.StdEncoding.EncodeToString(data))) {
		t.Fatal("image bytes persisted inside message")
	}
	called := false
	model := s.Resolve(transportFunc(func(ctx context.Context, req core.Request, emit func(core.Delta)) (core.Response, error) {
		called = true
		source := req.Messages[0].Content[0].Source.(map[string]any)
		if source["data"] != base64.StdEncoding.EncodeToString(data) || source["type"] != "base64" {
			t.Fatal("invalid hydrated image")
		}
		return core.Response{}, nil
	}))
	if _, err = model.Stream(context.Background(), core.Request{Messages: []core.Message{message}}, nil); err != nil {
		t.Fatal(err)
	}
	if !called || message.Content[0].Source.(map[string]any)["type"] != "dsgo_attachment" {
		t.Fatal("transport mutated the durable message")
	}
}
func TestTamperingAndEscapingReferencesFailClosed(t *testing.T) {
	s, _ := Open(t.TempDir())
	ref, err := s.Put([]byte("original"), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	id := ref["id"].(string)
	if err = os.Chmod(filepath.Join(s.path, id), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(s.path, id), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Read(id); err == nil {
		t.Fatal("accepted a corrupt object")
	}
	if _, err = s.Read("../../outside"); err == nil {
		t.Fatal("accepted path traversal")
	}
	os.Remove(filepath.Join(s.path, id))
	outside := filepath.Join(t.TempDir(), "image")
	os.WriteFile(outside, []byte("original"), 0600)
	if err = os.Symlink(outside, filepath.Join(s.path, id)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Read(id); err == nil {
		t.Fatal("accepted a symlink object")
	}
}
