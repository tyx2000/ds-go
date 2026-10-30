package model

import (
	"encoding/json"
	"os"
	"testing"
)

func TestDeepSeekImagePricingPinnedGolden(t *testing.T) {
	data, err := os.ReadFile("testdata/image-pricing-687f282.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Upstream string
		Cases    []struct {
			Width, Height        int
			Target               struct{ Width, Height int }
			Tokens, SourceTokens int
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Upstream != "687f282e8625c6b8d11ff58211a90524be2e4317" || len(fixture.Cases) < 500 {
		t.Fatal("invalid pinned fixture")
	}
	for _, item := range fixture.Cases {
		w, h, err := DeepSeekRequestImageDimensions(item.Width, item.Height)
		if err != nil || w != item.Target.Width || h != item.Target.Height {
			t.Fatalf("target %dx%d = %dx%d, %v; want %+v", item.Width, item.Height, w, h, err, item.Target)
		}
		tokens, err := DeepSeekImageTokens(w, h)
		if err != nil || tokens != item.Tokens {
			t.Fatalf("price %dx%d = %d, %v; want %d", w, h, tokens, err, item.Tokens)
		}
		tokens, err = DeepSeekImageTokens(item.Width, item.Height)
		if err != nil || tokens != item.SourceTokens {
			t.Fatalf("source price %dx%d = %d, %v; want %d", item.Width, item.Height, tokens, err, item.SourceTokens)
		}
	}
}

func TestDeepSeekImageGeometryRejectsInvalidInput(t *testing.T) {
	for _, dims := range [][2]int{{0, 1}, {1, 0}, {-1, 10}, {8193, 8192}} {
		if _, _, err := DeepSeekRequestImageDimensions(dims[0], dims[1]); err == nil {
			t.Fatalf("accepted %v", dims)
		}
		if _, err := DeepSeekImageTokens(dims[0], dims[1]); err == nil {
			t.Fatalf("priced %v", dims)
		}
	}
}
