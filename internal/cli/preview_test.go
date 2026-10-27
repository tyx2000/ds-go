package cli

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dsgo/internal/core"
)

func TestPreviewOptionsAndSourceNavigation(t *testing.T) {
	options, err := parsePreviewOptions(`a file.md {"line":2,"limit":3,"search":"match"}`)
	if err != nil || options.Path != "a file.md" || options.Line != 2 || options.Limit != 3 {
		t.Fatalf("options %+v %v", options, err)
	}
	for _, bad := range []string{`{"path":"a","plugin":"bad"}`, `{"path":"a"} {}`, `a {"path":"b"}`} {
		if _, err := parsePreviewOptions(bad); err == nil {
			t.Fatalf("accepted malformed preview options %s", bad)
		}
	}
	cwd := t.TempDir()
	if err = os.WriteFile(filepath.Join(cwd, "source.txt"), []byte("first\nmatch\x1b]52;bad\a\nthird\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := &App{ctx: context.Background(), session: core.Session{CWD: cwd}, out: &out, err: &out}
	if err = a.previewCommand(`source.txt {"line":2,"limit":1,"search":"match"}`); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b") || !strings.Contains(out.String(), "     2  match") || !strings.Contains(out.String(), "Next source line: 3") || !strings.Contains(out.String(), "Matching source lines: [2]") {
		t.Fatalf("unsafe or missing text navigation: %s", out.String())
	}
}

func TestPreviewRedirectedImageNeverWritesControlProtocols(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, img); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "image.png"), pngBytes.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERM", "xterm-kitty")
	t.Setenv("KITTY_WINDOW_ID", "fixture")
	var out bytes.Buffer
	a := &App{ctx: context.Background(), session: core.Session{CWD: cwd}, out: &out, err: &out}
	if err := a.previewCommand("image.png"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b") || !strings.Contains(out.String(), "inline display requires") {
		t.Fatalf("control bytes in redirected image output %q", out.String())
	}
	if err := a.previewCommand(`image.png {"protocol":"kitty"}`); err == nil {
		t.Fatal("forced image controls accepted on redirected output")
	}
	out.Reset()
	a.flags.json = true
	if err := a.previewCommand("image.png"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b") || !strings.Contains(out.String(), `"width": 4`) {
		t.Fatalf("invalid image metadata JSON %s", out.String())
	}
}
