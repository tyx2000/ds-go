package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"strings"
	"testing"

	"dsgo/internal/attachments"
	"dsgo/internal/core"
	"dsgo/internal/model"
	"dsgo/internal/state"
)

func projectionFixture(t *testing.T) (*App, core.Block) {
	t.Helper()
	store, err := attachments.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raster := image.NewNRGBA(image.Rect(0, 0, 1400, 1400))
	for i := 3; i < len(raster.Pix); i += 4 {
		raster.Pix[i] = 255
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, raster); err != nil {
		t.Fatal(err)
	}
	ref, err := store.SaveImage(context.Background(), encoded.Bytes(), "image/png", "图像 \"<&>.png")
	if err != nil {
		t.Fatal(err)
	}
	return &App{cfg: state.DefaultConfig(), attachmentStore: store}, core.Block{Type: "image", Source: ref.Source()}
}

func TestSCOR020ImagePricingMatchesActualRequestAndPreservesHistory(t *testing.T) {
	a, block := projectionFixture(t)
	ctx := context.Background()
	request := core.Request{Model: "deepseek-flash", Provider: "deepseek-official", Messages: []core.Message{{Role: "user", Content: []core.Block{block}}}}
	before := string(core.JSON(request))
	route, err := a.contextRoute(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if route.ImagePricing == nil || route.FileText == nil {
		t.Fatal("production route has no media pricing")
	}
	prices, err := route.ImagePricing([]core.Block{block, block})
	if err != nil || len(prices) != 2 {
		t.Fatalf("prices: %v, %v", prices, err)
	}
	var received core.Request
	wrapped := a.attachmentStore.ResolveProjected(lifecycleModel(func(_ context.Context, req core.Request, _ func(core.Delta)) (core.Response, error) {
		received = req
		return core.Response{}, nil
	}), a.projectRequestImage)
	if _, err := wrapped.Stream(ctx, request, func(core.Delta) {}); err != nil {
		t.Fatal(err)
	}
	blocks := received.Messages[0].Content
	if len(blocks) != 2 || blocks[0].Type != "text" || blocks[1].Type != "image" || prices[0].Text != blocks[0].Text || prices[1] != prices[0] {
		t.Fatalf("projected blocks and prices differ: %+v; %+v", blocks, prices)
	}
	var source struct{ Data string }
	if err := json.Unmarshal(core.JSON(blocks[1].Source), &source); err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(source.Data)
	if err != nil {
		t.Fatal(err)
	}
	facts, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	w, h, _ := model.DeepSeekRequestImageDimensions(1400, 1400)
	if facts.Width != w || facts.Height != h {
		t.Fatalf("sent %dx%d; priced target %dx%d", facts.Width, facts.Height, w, h)
	}
	tokens, _ := model.DeepSeekImageTokens(facts.Width, facts.Height)
	if tokens != prices[0].VisualTokens || tokens <= 16 {
		t.Fatalf("wire price=%d; route price=%+v", tokens, prices[0])
	}
	if !strings.Contains(blocks[0].Text, "read-only") {
		t.Fatal("missing source access handle")
	}
	if string(core.JSON(request)) != before {
		t.Fatal("projection mutated canonical request/history")
	}
}

func TestSCOR020TextOnlyAndGenericRouteProjection(t *testing.T) {
	a, block := projectionFixture(t)
	ctx := context.Background()
	for _, name := range []string{"deepseek-v4-pro", "uncatalogued"} {
		req := core.Request{Model: name, Messages: []core.Message{{Role: "user", Content: []core.Block{block}}}}
		route, err := a.contextRoute(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		prices, err := route.ImagePricing([]core.Block{block})
		if err != nil || len(prices) != 1 || prices[0].VisualTokens != 0 {
			t.Fatalf("text-only price %+v, %v", prices, err)
		}
		wrapped := a.attachmentStore.ResolveProjected(lifecycleModel(func(_ context.Context, sent core.Request, _ func(core.Delta)) (core.Response, error) {
			content := sent.Messages[0].Content
			if len(content) != 1 || content[0].Type != "text" || content[0].Text != prices[0].Text {
				t.Fatalf("text-only wire differs: %+v", content)
			}
			return core.Response{}, nil
		}), a.projectRequestImage)
		if _, err := wrapped.Stream(ctx, req, func(core.Delta) {}); err != nil {
			t.Fatal(err)
		}
	}
	a.cfg.SelectedProvider = "generic"
	route, err := a.contextRoute(ctx, core.Request{Model: "some-model"})
	if err != nil || route.ImagePricing != nil || route.FileText == nil || route.ContextWindow != a.cfg.ContextTokens {
		t.Fatalf("generic route contract: %+v, %v", route, err)
	}
}

func TestSCOR020NestedLiveReplayAndPrunedMediaUseIdenticalProjection(t *testing.T) {
	a, imageBlock := projectionFixture(t)
	file := core.Block{Type: "file", Source: map[string]any{"attachmentId": "sha256:" + strings.Repeat("a", 64), "name": "报告.pdf", "bytes": 123}}
	base := []core.Block{{Type: "text", Text: "original result"}, imageBlock, file}
	var replay []any
	if err := json.Unmarshal(core.JSON(base), &replay); err != nil {
		t.Fatal(err)
	}
	var pruned []json.RawMessage
	if err := json.Unmarshal(core.JSON(base), &pruned); err != nil {
		t.Fatal(err)
	}
	var native []map[string]any
	if err := json.Unmarshal(core.JSON(base), &native); err != nil {
		t.Fatal(err)
	}
	var expected string
	for _, content := range []any{base, replay, pruned, json.RawMessage(core.JSON(base)), []any{base[0], imageBlock, file}, native} {
		req := core.Request{Model: "deepseek-flash", Messages: []core.Message{{Role: "user", Content: []core.Block{{Type: "tool_result", ToolUseID: "read", Content: content}}}}}
		before := string(core.JSON(req))
		wrapped := a.attachmentStore.ResolveProjected(lifecycleModel(func(_ context.Context, sent core.Request, _ func(core.Delta)) (core.Response, error) {
			actual := string(core.JSON(sent.Messages[0].Content[0].Content))
			var blocks []core.Block
			if err := json.Unmarshal([]byte(actual), &blocks); err != nil {
				t.Fatal(err)
			}
			canonical := string(core.JSON(blocks))
			if expected == "" {
				expected = canonical
			} else if canonical != expected {
				t.Fatal("live, replay, pruned, and typed content project differently")
			}
			if len(blocks) != 4 || blocks[2].Type != "image" || blocks[3].Type != "text" {
				t.Fatalf("media projection lost blocks: %+v", blocks)
			}
			text, err := attachments.FileHandleText(file)
			if err != nil || blocks[3].Text != text {
				t.Fatalf("file projection differs from meter: %v", err)
			}
			return core.Response{}, nil
		}), a.projectRequestImage)
		if _, err := wrapped.Stream(context.Background(), req, func(core.Delta) {}); err != nil {
			t.Fatal(err)
		}
		if string(core.JSON(req)) != before {
			t.Fatal("nested projection mutated canonical history")
		}
	}
}

func TestSCOR020LegacyImageReferencesRemainReadable(t *testing.T) {
	a, block := projectionFixture(t)
	ref, _, err := a.attachmentStore.ImageReference(context.Background(), block)
	if err != nil {
		t.Fatal(err)
	}
	data, err := a.attachmentStore.Read(strings.TrimPrefix(ref.AttachmentID, "sha256:"))
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []any{
		map[string]any{"type": "dsgo_attachment", "id": strings.TrimPrefix(ref.AttachmentID, "sha256:"), "media_type": ref.MediaType},
		map[string]any{"type": "base64", "data": base64.StdEncoding.EncodeToString(data), "media_type": ref.MediaType},
	} {
		got, _, err := a.attachmentStore.ImageReference(context.Background(), core.Block{Type: "image", Source: source})
		if err != nil || got.Width != ref.Width || got.Height != ref.Height || got.Bytes != len(data) {
			t.Fatalf("legacy facts=%+v, %v", got, err)
		}
	}
}
