package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dsgo/internal/attachments"
	"dsgo/internal/core"
	"dsgo/internal/mcp"
	"dsgo/internal/state"
)

func TestMCPImagesCanonicalAndDurableProjection(t *testing.T) {
	a, s := contractsApp(t)
	a.cfg = state.DefaultConfig()
	a.attachmentStore, _ = attachments.Open(a.store.Root())
	var b bytes.Buffer
	png.Encode(&b, image.NewGray(image.Rect(0, 0, 3, 2)))
	encoded := base64.StdEncoding.EncodeToString(b.Bytes())
	raw := mcp.ToolResult{Content: []json.RawMessage{core.JSON(map[string]any{"type": "text", "text": "before"}), core.JSON(map[string]any{"type": "image", "mimeType": "image/png", "data": encoded}), core.JSON(map[string]any{"type": "text", "text": "after"})}}
	result := a.projectMCPResult(context.Background(), contractsContext(a, s), "image", raw)
	if result.IsError || len(result.Blocks) != 3 || result.Blocks[1].Type != "image" {
		t.Fatalf("projection: %+v", result)
	}
	if !bytes.Contains(result.Value, []byte(encoded)) {
		t.Fatal("canonical value was discarded")
	}
	stored := string(core.JSON(result))
	if strings.Contains(stored, encoded) || strings.Contains(stored, `"data":`) {
		t.Fatal("inline media entered durable result")
	}
	ref := result.Blocks[1].Source.(map[string]any)
	if _, err := a.attachmentStore.Read(ref["id"].(string)); err != nil {
		t.Fatal(err)
	}
}

func TestMCPImageBatchRefusalAndErrorDoNotPublishImages(t *testing.T) {
	for _, isError := range []bool{false, true} {
		a, s := contractsApp(t)
		a.cfg = state.DefaultConfig()
		a.attachmentStore, _ = attachments.Open(a.store.Root())
		var b bytes.Buffer
		png.Encode(&b, image.NewGray(image.Rect(0, 0, 3, 2)))
		blocks := []json.RawMessage{core.JSON(map[string]any{"type": "image", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(b.Bytes())})}
		if !isError {
			blocks = append(blocks, core.JSON(map[string]any{"type": "image", "mimeType": "image/png", "data": "INVALID"}))
		}
		result := a.projectMCPResult(context.Background(), contractsContext(a, s), "images", mcp.ToolResult{Content: blocks, IsError: isError})
		for _, block := range result.Blocks {
			if block.Type == "image" {
				t.Fatal("refused batch projected an image")
			}
		}
		entries, err := os.ReadDir(filepath.Join(a.store.Root(), "attachments"))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("refused batch published %d objects", len(entries))
		}
		if isError && len(result.Value) != 0 {
			t.Fatal("error result exposed canonical success")
		}
	}
}
