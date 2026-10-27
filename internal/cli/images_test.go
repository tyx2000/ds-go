package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dsgo/internal/attachments"
	"dsgo/internal/state"
)

func TestReadImageOfficialContractExtensionlessAndPersistentReference(t *testing.T) {
	a, session := contractsApp(t)
	a.cfg = state.DefaultConfig()
	var err error
	a.attachmentStore, err = attachments.Open(a.store.Root())
	if err != nil {
		t.Fatal(err)
	}
	if err = a.registerImageTool(); err != nil {
		t.Fatal(err)
	}
	raster := image.NewNRGBA(image.Rect(0, 0, 4, 3))
	for i := 3; i < len(raster.Pix); i += 4 {
		raster.Pix[i] = 255
	}
	raster.Set(1, 1, color.NRGBA{250, 40, 100, 255})
	var encoded bytes.Buffer
	if err = png.Encode(&encoded, raster); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(session.CWD, "no-extension")
	if err = os.WriteFile(path, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := a.registry.Execute(context.Background(), contractsContext(a, session), "read_image", json.RawMessage(`{"file_path":"no-extension"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Blocks) != 2 || result.Blocks[1].Type != "image" || !strings.Contains(result.Content, "image/png image, 4x3 px") {
		t.Fatalf("result=%#v", result)
	}
	source, ok := result.Blocks[1].Source.(map[string]any)
	if !ok || source["type"] != "dsgo_attachment" || source["data"] != nil {
		t.Fatalf("non-durable image=%#v", source)
	}
	if _, err = a.attachmentStore.Read(source["id"].(string)); err != nil {
		t.Fatal("returned before attachment commit: ", err)
	}
	if extension := imageExtension(".hidden"); extension != "" {
		t.Fatalf("dotfile extension=%q", extension)
	}
}

func TestReadImageRefusesUnknownRouteBeforeReadingFile(t *testing.T) {
	a, session := contractsApp(t)
	a.cfg = state.DefaultConfig()
	a.attachmentStore, _ = attachments.Open(a.store.Root())
	for _, name := range []string{"deepseek-v4-pro", "unknown-model"} {
		session.Model = name
		if err := a.store.Update(session); err != nil {
			t.Fatal(err)
		}
		_, err := a.readImage(context.Background(), contractsContext(a, session), json.RawMessage(`{"file_path":"missing.png"}`))
		if err == nil || !strings.Contains(err.Error(), "does not declare image input") {
			t.Fatalf("route %s performed file read: %v", name, err)
		}
	}
	session.Model = "deepseek-flash"
	_ = a.store.Update(session)
	for _, raw := range []string{`{}`, `{"file_path":" "}`, `{"file_path":"file.txt"}`, `{"file_path":"file.png","size":256}`} {
		if _, err := a.readImage(context.Background(), contractsContext(a, session), json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestImageReadEnvelopeReportsCoordinateScaling(t *testing.T) {
	ref := attachments.ImageRef{MediaType: "image/jpeg", Width: 100, Height: 50, Bytes: 200, OriginalDimensions: &attachments.ImageDimensions{Width: 200, Height: 100}}
	got := formatImageReadOutput("/a.png", ref)
	if !strings.Contains(got, "downscaled from 200x100 px; multiply coordinates by 2.00 to locate features") {
		t.Fatalf("envelope=%s", got)
	}
	ref.OriginalDimensions.Height = 99
	got = formatImageReadOutput("/a.png", ref)
	if !strings.Contains(got, "multiply x coordinates by 2.00 and y coordinates by 1.98") {
		t.Fatalf("axes envelope=%s", got)
	}
}
