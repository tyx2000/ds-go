package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"dsgo/internal/core"
)

func groupEntry(name string) Registration {
	return Registration{Spec: core.ToolSpec{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)}, Run: func(context.Context, core.ToolContext, json.RawMessage) (core.ToolResult, error) {
		return core.ToolResult{Content: name}, nil
	}}
}

func TestDiscoveryGroupRollbackAndDisposal(t *testing.T) {
	r := New()
	defer r.Close()
	if err := r.ReplaceGroup("mcp:test", []Registration{groupEntry("remote_old")}); err != nil {
		t.Fatal(err)
	}
	if err := r.ReplaceGroup("mcp:test", []Registration{groupEntry("duplicate"), groupEntry("duplicate")}); err == nil {
		t.Fatal("duplicate discovery accepted")
	}
	if _, err := r.Execute(context.Background(), core.ToolContext{}, "remote_old", json.RawMessage(`{}`)); err != nil {
		t.Fatal("discovery rejection removed last known tool", err)
	}
	bad := groupEntry("invalid")
	bad.Spec.InputSchema = json.RawMessage(`{"type":"not-a-type"}`)
	if err := r.ReplaceGroup("mcp:test", []Registration{bad}); err == nil {
		t.Fatal("invalid schema accepted")
	}
	if _, err := r.Execute(context.Background(), core.ToolContext{}, "remote_old", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := r.ReplaceGroup("mcp:test", []Registration{groupEntry("remote_partial"), groupEntry("read")}); err == nil {
		t.Fatal("foreign collision accepted")
	}
	for _, name := range []string{"remote_old", "remote_partial"} {
		if _, err := r.Execute(context.Background(), core.ToolContext{}, name, json.RawMessage(`{}`)); err == nil {
			t.Fatalf("failed generation left %s", name)
		}
	}
	for _, spec := range r.Specs() {
		if spec.Name == "read" {
			return
		}
	}
	t.Fatal("foreign tool removed")
}

func TestDiscoveryGroupPublicationIsAtomic(t *testing.T) {
	r := New()
	defer r.Close()
	entries := func(prefix string) []Registration {
		return []Registration{groupEntry("remote_" + prefix + "_a"), groupEntry("remote_" + prefix + "_b")}
	}
	if err := r.ReplaceGroup("mcp:test", entries("one")); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			seen := []string{}
			for _, spec := range r.Specs() {
				if strings.HasPrefix(spec.Name, "remote_") {
					seen = append(seen, spec.Name)
				}
			}
			if len(seen) != 2 || strings.TrimSuffix(seen[0], "_a") != strings.TrimSuffix(seen[1], "_b") {
				t.Errorf("partial publication: %v", seen)
				return
			}
		}
	}()
	for i := 0; i < 30; i++ {
		prefix := "one"
		if i%2 == 0 {
			prefix = "two"
		}
		if err := r.ReplaceGroup("mcp:test", entries(prefix)); err != nil {
			t.Error(err)
			break
		}
	}
	close(done)
	wg.Wait()
	if err := r.ReplaceGroup("mcp:test", nil); err != nil {
		t.Fatal(err)
	}
	for _, spec := range r.Specs() {
		if strings.HasPrefix(spec.Name, "remote_") {
			t.Fatal("disposed tool remains")
		}
	}
}

func TestMCPGroupLeavesInputSchemaValidationToServer(t *testing.T) {
	r := New()
	defer r.Close()
	entry := groupEntry("remote_schema")
	entry.Spec.InputSchema = json.RawMessage(`{"type":"object","$schema":"https://example.invalid/dialect","$ref":"https://example.invalid/schema","properties":{"value":{"type":"integer"}}}`)
	entry.InputValidatedByServer = true
	var received string
	entry.Run = func(_ context.Context, _ core.ToolContext, input json.RawMessage) (core.ToolResult, error) {
		received = string(input)
		return core.ToolResult{Content: "server accepted"}, nil
	}
	if err := r.ReplaceGroup("mcp:test", []Registration{entry}); err != nil {
		t.Fatal("MCP schema was compiled locally", err)
	}
	input := json.RawMessage(`{"value":"server-owned constraint"}`)
	if _, err := r.Execute(context.Background(), core.ToolContext{}, entry.Spec.Name, input); err != nil || received != string(input) {
		t.Fatalf("input did not reach server: %q, %v", received, err)
	}
	entry.InputValidatedByServer = false
	if err := r.ReplaceGroup("builtin:checked", []Registration{entry}); err == nil {
		t.Fatal("server-validation flag leaked to other registrations")
	}
}
