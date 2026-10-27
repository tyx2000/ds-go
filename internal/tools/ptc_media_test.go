package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"dsgo/internal/core"
)

func TestPTCCanonicalMediaStaysExecutionLocal(t *testing.T) {
	for _, mode := range []string{"per-call", "persistent-in-memory"} {
		t.Run(mode, func(t *testing.T) {
			r := New()
			defer r.Close()
			r.SetPTCMode(mode)
			const inline = "aW5saW5lLW1lZGlhLXNob3VsZC1zdGF5LWxvY2Fs"
			if err := r.Register(spec("media_fixture", "MCP-shaped canonical fixture", `{"type":"object"}`), func(context.Context, core.ToolContext, json.RawMessage) (core.ToolResult, error) {
				return core.ToolResult{Content: "image", Value: core.JSON(map[string]any{"content": []any{map[string]any{"type": "image", "data": inline}}}), Blocks: []core.Block{{Type: "image", Source: map[string]any{"type": "dsgo_attachment", "id": strings.Repeat("a", 64), "media_type": "image/png"}}}}, nil
			}); err != nil {
				t.Fatal(err)
			}
			var audit []byte
			tc := core.ToolContext{SessionID: "media", CWD: t.TempDir(), Permission: "full-access", Emit: func(_ string, value any) error { audit = append(audit, core.JSON(value)...); return nil }}
			code := `const value=await tools.media_fixture({});return value.content[0].data.length;`
			if mode == "persistent-in-memory" {
				code = `const value=await tools.media_fixture({});value.content[0].data.length;`
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := r.Execute(ctx, tc, "run_code", core.JSON(map[string]any{"code": code}))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(audit), inline) || strings.Contains(string(core.JSON(result)), inline) {
				t.Fatal("canonical inline media escaped into durable output")
			}
			if !strings.Contains(result.Content, `"value":`) {
				t.Fatal("program lost canonical result")
			}
			found := false
			for _, block := range result.Blocks {
				found = found || block.Type == "image"
			}
			if !found {
				t.Fatal("durable media was not promoted from nested tool")
			}
		})
	}
}
