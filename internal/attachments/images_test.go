package attachments

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"dsgo/internal/core"
)

func fixturePNG(t *testing.T, width, height int, alpha bool) []byte {
	t.Helper()
	raster := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			a := uint8(255)
			if alpha && x == 0 {
				a = 96
			}
			raster.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 17), G: uint8(y * 23), B: 96, A: a})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, raster); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestSaveImageCleanPassThroughAndMetadata(t *testing.T) {
	s, _ := Open(t.TempDir())
	data := fixturePNG(t, 9, 7, true)
	ref, err := s.SaveImage(context.Background(), data, "image/png", `C:\private\example.png`)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Width != 9 || ref.Height != 7 || ref.Bytes != len(data) || ref.Name != "example.png" || ref.MediaType != "image/png" || ref.OriginalDimensions != nil {
		t.Fatalf("ref=%#v", ref)
	}
	stored, err := s.Read(strings.TrimPrefix(ref.AttachmentID, "sha256:"))
	if err != nil || !bytes.Equal(stored, data) {
		t.Fatalf("clean raster changed: %v", err)
	}
	for _, declared := range []string{"image/jpeg", "image/gif", "image/webp"} {
		_, err = s.SaveImage(context.Background(), data, declared, "wrong")
		var e *ImageError
		if !errors.As(err, &e) || e.Code != "IMAGE_TYPE_MISMATCH" {
			t.Fatalf("accepted mismatch %s: %v", declared, err)
		}
	}
	if name := imageDisplayName("/private/a\x00\x1b.png"); name != "a.png" {
		t.Fatalf("name=%q", name)
	}
}

func TestSaveImageLimitsAndFullDecodeBeforePublication(t *testing.T) {
	s, _ := Open(t.TempDir())
	header := func(w, h uint32) []byte {
		out := append([]byte(nil), []byte{137, 80, 78, 71, 13, 10, 26, 10}...)
		data := make([]byte, 13)
		binary.BigEndian.PutUint32(data[:4], w)
		binary.BigEndian.PutUint32(data[4:8], h)
		data[8] = 8
		data[9] = 2
		chunk := append([]byte("IHDR"), data...)
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], 13)
		out = append(out, size[:]...)
		out = append(out, chunk...)
		binary.BigEndian.PutUint32(size[:], crc32.ChecksumIEEE(chunk))
		return append(out, size[:]...)
	}
	for _, test := range []struct {
		data []byte
		code string
	}{{nil, "INVALID_IMAGE"}, {header(8193, 1), "IMAGE_DIMENSION_TOO_LARGE"}, {header(8192, 8192), "IMAGE_TOO_MANY_PIXELS"}, {header(20, 20), "INVALID_IMAGE"}, {make([]byte, MaxImageBytes+1), "IMAGE_TOO_LARGE"}} {
		_, err := s.SaveImage(context.Background(), test.data, "image/png", "test.png")
		var e *ImageError
		if !errors.As(err, &e) || e.Code != test.code {
			t.Errorf("got %v, want %s", err, test.code)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.SaveImage(ctx, fixturePNG(t, 2, 2, false), "image/png", "cancel.png"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
}

func TestSaveImagesPreflightPublishesOnlyCompleteValidBatch(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	valid := ImageInput{Data: fixturePNG(t, 3, 4, false), MediaType: "image/png", Name: "valid.png"}
	assertEmpty := func() {
		t.Helper()
		entries, err := os.ReadDir(store.path)
		if err != nil || len(entries) != 0 {
			t.Fatalf("invalid batch published an earlier image: entries=%v err=%v", entries, err)
		}
	}
	if _, err = store.SaveImages(context.Background(), []ImageInput{valid, {Data: []byte("broken"), MediaType: "image/png"}}); err == nil {
		t.Fatal("invalid second image was admitted")
	}
	assertEmpty()
	tooMany := make([]ImageInput, MaxImagesPerMessage+1)
	for i := range tooMany {
		tooMany[i] = valid
	}
	if _, err = store.SaveImages(context.Background(), tooMany); err == nil {
		t.Fatal("image-count limit was not enforced")
	}
	assertEmpty()
	large := make([]byte, 11<<20)
	tooLarge := make([]ImageInput, 20)
	for i := range tooLarge {
		tooLarge[i] = ImageInput{Data: large, MediaType: "image/png"}
	}
	_, err = store.SaveImages(context.Background(), tooLarge)
	var refusal *ImageError
	if !errors.As(err, &refusal) || refusal.Code != "MESSAGE_IMAGES_TOO_LARGE" {
		t.Fatalf("aggregate bound must run before any decoding: %v", err)
	}
	assertEmpty()
	refs, err := store.SaveImages(context.Background(), []ImageInput{valid, valid})
	if err != nil || len(refs) != 2 || refs[0].AttachmentID == "" || refs[0].AttachmentID != refs[1].AttachmentID || refs[0].Name != "valid.png" {
		t.Fatalf("valid ordered batch failed: %#v %v", refs, err)
	}
	for _, ref := range refs {
		if _, err = store.Read(strings.TrimPrefix(ref.AttachmentID, "sha256:")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestImageNormalizationGeometryAndOpaqueQualityRoute(t *testing.T) {
	for _, test := range []struct{ w, h, x, y int }{{10, 20, 10, 20}, {4096, 2048, 2896, 1448}, {2048, 4096, 1448, 2896}, {8192, 1, 8192, 1}} {
		w, h := NormalizedDimensions(test.w, test.h)
		if w != test.x || h != test.y || w*h > NormalizedMaxPixels {
			t.Errorf("%dx%d -> %dx%d, want %dx%d", test.w, test.h, w, h, test.x, test.y)
		}
	}
	s, _ := Open(t.TempDir())
	source := fixturePNG(t, 2100, 2100, false)
	ref, err := s.SaveImage(context.Background(), source, "image/png", "large.png")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Width != 2048 || ref.Height != 2048 || ref.MediaType != "image/jpeg" || ref.OriginalDimensions == nil || ref.OriginalDimensions.Width != 2100 {
		t.Fatalf("normalized=%#v", ref)
	}
	data, err := s.Read(strings.TrimPrefix(ref.AttachmentID, "sha256:"))
	if err != nil {
		t.Fatal(err)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != "jpeg" || config.Width != 2048 || config.Height != 2048 {
		t.Fatalf("bytes=%s %#v %v", format, config, err)
	}
}

func TestImageNormalizationTransparentGIFUsesWebP(t *testing.T) {
	if _, err := exec.LookPath("cwebp"); err != nil {
		t.Skip("native transparent WebP encoder unavailable")
	}
	raster := image.NewPaletted(image.Rect(0, 0, 8, 8), color.Palette{color.NRGBA{0, 0, 0, 0}, color.NRGBA{220, 40, 100, 255}})
	for i := 0; i < len(raster.Pix); i++ {
		raster.Pix[i] = uint8(i % 2)
	}
	var encoded bytes.Buffer
	if err := gif.Encode(&encoded, raster, nil); err != nil {
		t.Fatal(err)
	}
	s, _ := Open(t.TempDir())
	ref, err := s.SaveImage(context.Background(), encoded.Bytes(), "image/gif", "alpha.gif")
	if err != nil {
		t.Fatal(err)
	}
	if ref.MediaType != "image/webp" || ref.Width != 8 || ref.Height != 8 {
		t.Fatalf("ref=%#v", ref)
	}
	data, _ := s.Read(strings.TrimPrefix(ref.AttachmentID, "sha256:"))
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, alpha := decoded.At(0, 0).RGBA()
	if alpha != 0 {
		t.Fatal("transparent pixel was flattened")
	}
	// Wrap that still frame in an animated container with a larger canvas.
	// Only its first composited frame belongs in the durable image contract.
	var frameChunks []byte
	for p := 12; p+8 <= len(data); {
		size := int(binary.LittleEndian.Uint32(data[p+4 : p+8]))
		end := p + 8 + size + (size & 1)
		if end > len(data) {
			t.Fatal("bad encoder chunk")
		}
		kind := string(data[p : p+4])
		if kind == "ALPH" || kind == "VP8 " || kind == "VP8L" {
			frameChunks = append(frameChunks, data[p:end]...)
		}
		p = end
	}
	canvasHeader := make([]byte, 10)
	canvasHeader[0] = 0x12
	canvasHeader[4] = 11
	canvasHeader[7] = 7
	frameHeader := make([]byte, 16)
	frameHeader[0] = 1
	frameHeader[6] = 7
	frameHeader[9] = 7
	frameHeader[12] = 20
	frameHeader[15] = 2
	body := append([]byte("WEBP"), webpChunk("VP8X", canvasHeader)...)
	body = append(body, webpChunk("ANIM", make([]byte, 6))...)
	body = append(body, webpChunk("ANMF", append(frameHeader, frameChunks...))...)
	animated := append([]byte("RIFF"), make([]byte, 4)...)
	binary.LittleEndian.PutUint32(animated[4:8], uint32(len(body)))
	animated = append(animated, body...)
	staticRef, err := s.SaveImage(context.Background(), animated, "image/webp", "animation.webp")
	if err != nil {
		t.Fatal(err)
	}
	if staticRef.Width != 12 || staticRef.Height != 8 || staticRef.MediaType != "image/webp" {
		t.Fatalf("animation ref=%#v", staticRef)
	}
	staticBytes, _ := s.Read(strings.TrimPrefix(staticRef.AttachmentID, "sha256:"))
	if bytes.Contains(staticBytes, []byte("ANMF")) {
		t.Fatal("animation remained in normalized storage")
	}
}

func TestGIFFirstFramePreservesLogicalCanvasAndOffset(t *testing.T) {
	palette := color.Palette{color.NRGBA{255, 255, 255, 255}, color.NRGBA{255, 0, 0, 255}}
	frame := image.NewPaletted(image.Rect(2, 3, 10, 9), palette)
	for i := range frame.Pix {
		frame.Pix[i] = 1
	}
	var encoded bytes.Buffer
	if err := gif.EncodeAll(&encoded, &gif.GIF{Image: []*image.Paletted{frame}, Delay: []int{0}, BackgroundIndex: 0, Config: image.Config{ColorModel: palette, Width: 12, Height: 12}}); err != nil {
		t.Fatal(err)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	raster, err := decodeRaster(encoded.Bytes(), "gif", config)
	if err != nil {
		t.Fatal(err)
	}
	if raster.Bounds() != image.Rect(0, 0, 12, 12) {
		t.Fatalf("GIF logical canvas was lost: %v", raster.Bounds())
	}
	if got := color.NRGBAModel.Convert(raster.At(0, 0)).(color.NRGBA); got != (color.NRGBA{255, 255, 255, 255}) {
		t.Fatalf("background changed: %v", got)
	}
	if got := color.NRGBAModel.Convert(raster.At(2, 3)).(color.NRGBA); got != (color.NRGBA{255, 0, 0, 255}) {
		t.Fatalf("frame offset changed: %v", got)
	}
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.SaveImage(context.Background(), encoded.Bytes(), "image/gif", "offset.gif")
	if err != nil || ref.Width != 12 || ref.Height != 12 {
		t.Fatalf("durable GIF dimensions: %#v %v", ref, err)
	}
}

func TestGIFFirstFramePreservesTransparentCanvas(t *testing.T) {
	palette := color.Palette{color.NRGBA{0, 0, 0, 0}, color.NRGBA{255, 0, 0, 255}}
	frame := image.NewPaletted(image.Rect(2, 3, 10, 9), palette)
	for i := range frame.Pix {
		frame.Pix[i] = 1
	}
	frame.SetColorIndex(3, 4, 0)
	var encoded bytes.Buffer
	if err := gif.EncodeAll(&encoded, &gif.GIF{Image: []*image.Paletted{frame}, Delay: []int{0}, Config: image.Config{ColorModel: palette, Width: 12, Height: 12}}); err != nil {
		t.Fatal(err)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	raster, err := decodeRaster(encoded.Bytes(), "gif", config)
	if err != nil {
		t.Fatal(err)
	}
	if raster.Bounds() != image.Rect(0, 0, 12, 12) {
		t.Fatalf("logical bounds: %v", raster.Bounds())
	}
	for _, point := range []image.Point{{0, 0}, {3, 4}, {11, 11}} {
		if _, _, _, alpha := raster.At(point.X, point.Y).RGBA(); alpha != 0 {
			t.Fatalf("transparent canvas/pixel flattened at %v", point)
		}
	}
	if got := color.NRGBAModel.Convert(raster.At(2, 3)).(color.NRGBA); got != (color.NRGBA{255, 0, 0, 255}) {
		t.Fatalf("offset content changed: %v", got)
	}
}

func TestImageOrientationAndNestedReferenceHydration(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 2, 3))
	source.SetNRGBA(0, 0, color.NRGBA{255, 0, 0, 255})
	source.SetNRGBA(1, 2, color.NRGBA{0, 255, 0, 255})
	rotated := orientImage(source, 6)
	if rotated.Bounds().Dx() != 3 || rotated.Bounds().Dy() != 2 || rotated.At(2, 0) != source.At(0, 0) {
		t.Fatal("EXIF orientation 6 was not applied")
	}
	var encoded bytes.Buffer
	_ = jpeg.Encode(&encoded, source, nil)
	// Little-endian TIFF IFD with orientation=6.
	exif := []byte{'E', 'x', 'i', 'f', 0, 0, 'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0, 0x12, 1, 3, 0, 1, 0, 0, 0, 6, 0, 0, 0, 0, 0, 0, 0}
	segment := []byte{0xff, 0xe1, 0, byte(len(exif) + 2)}
	segment = append(segment, exif...)
	jpegData := append(append(append([]byte{}, encoded.Bytes()[:2]...), segment...), encoded.Bytes()[2:]...)
	if jpegOrientation(jpegData) != 6 {
		t.Fatal("failed to parse EXIF orientation")
	}
	s, _ := Open(t.TempDir())
	ref, err := s.SaveImage(context.Background(), jpegData, "image/jpeg", "oriented.jpg")
	if err != nil || ref.Width != 3 || ref.Height != 2 {
		t.Fatalf("orientation=%#v %v", ref, err)
	}
	message := core.Message{Role: "user", Content: []core.Block{{Type: "tool_result", ToolUseID: "read-image", Content: []core.Block{{Type: "text", Text: "image"}, {Type: "image", Source: ref.Source()}}}}}
	before, _ := json.Marshal(message)
	var replay core.Message
	if err = json.Unmarshal(before, &replay); err != nil {
		t.Fatal(err)
	}
	for _, input := range []core.Message{message, replay} {
		called := false
		transport := s.Resolve(transportFunc(func(ctx context.Context, request core.Request, _ func(core.Delta)) (core.Response, error) {
			called = true
			content := request.Messages[0].Content[0].Content
			blob, _ := json.Marshal(content)
			if bytes.Contains(blob, []byte("dsgo_attachment")) || !bytes.Contains(blob, []byte(`"type":"base64"`)) {
				t.Fatalf("nested content not hydrated: %s", blob)
			}
			return core.Response{}, nil
		}))
		if _, err = transport.Stream(context.Background(), core.Request{Messages: []core.Message{input}}, nil); err != nil || !called {
			t.Fatalf("transport=%v called=%v", err, called)
		}
	}
	after, _ := json.Marshal(message)
	if !bytes.Equal(before, after) || bytes.Contains(after, []byte(base64.StdEncoding.EncodeToString(jpegData))) {
		t.Fatal("hydration mutated durable history")
	}
}

func TestConcurrentAttachmentPublicationNeverObservesPartialObject(t *testing.T) {
	s, _ := Open(t.TempDir())
	data := bytes.Repeat([]byte("same image bytes"), 16000)
	var group sync.WaitGroup
	failures := make(chan error, 16)
	for range 16 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := s.Put(data, "image/png")
			if err != nil {
				failures <- err
			}
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
}
