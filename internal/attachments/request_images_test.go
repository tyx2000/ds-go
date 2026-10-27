package attachments

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRequestImageIdentityTargetsAllTransformInputs(t *testing.T) {
	ref := ImageRef{AttachmentID: "sha256:" + strings.Repeat("0", 64)}
	target := RequestImageTarget{Width: 100, Height: 50, MaxBytes: 1234}
	id, err := RequestImageVariantID(ref, target)
	if err != nil {
		t.Fatal(err)
	}
	// Independently computed digest of the canonical request-image-v6 descriptor.
	const want = "sha256:a427a209524ffbdb28202fe92c176b62621013331a55bcbf61762218ff6bd6d2"
	if id != want {
		t.Fatalf("descriptor identity changed: %s", id)
	}
	for _, changed := range []RequestImageTarget{{Width: 99, Height: 50, MaxBytes: 1234}, {Width: 100, Height: 49, MaxBytes: 1234}, {Width: 100, Height: 50, MaxBytes: 1233}} {
		other, err := RequestImageVariantID(ref, changed)
		if err != nil || other == id {
			t.Fatalf("target not part of identity: %s %v", other, err)
		}
	}
	for _, invalid := range []RequestImageTarget{{Width: 0, Height: 10, MaxBytes: 10}, {Width: 10, Height: -1, MaxBytes: 10}, {Width: 10, Height: 10, MaxBytes: 0}} {
		if _, err := RequestImageVariantID(ref, invalid); err == nil {
			t.Fatalf("invalid target accepted: %+v", invalid)
		}
	}
}

func TestRequestImagePassThroughResizeCacheAndRefIntegrity(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := fixturePNG(t, 160, 80, false)
	ref, err := store.SaveImage(context.Background(), source, "image/png", "source.png")
	if err != nil {
		t.Fatal(err)
	}
	pass, err := store.RequestImage(context.Background(), ref, RequestImageTarget{Width: 160, Height: 80, MaxBytes: 1 << 20})
	if err != nil || !bytes.Equal(pass.Data, source) || pass.MediaType != "image/png" || pass.Width != 160 || pass.Height != 80 {
		t.Fatalf("pass-through changed: %#v %v", pass, err)
	}
	target := RequestImageTarget{Width: 64, Height: 32, MaxBytes: 1 << 20}
	first, err := store.RequestImage(context.Background(), ref, target)
	if err != nil {
		t.Fatal(err)
	}
	if first.Width != 64 || first.Height != 32 || first.MediaType != "image/jpeg" || first.Bytes != len(first.Data) {
		t.Fatalf("route dimensions: %#v", first)
	}
	stored, err := store.Read(strings.TrimPrefix(ref.AttachmentID, "sha256:"))
	if err != nil || !bytes.Equal(stored, source) {
		t.Fatalf("request transform modified durable bytes: %v", err)
	}
	hash := strings.TrimPrefix(first.VariantID, "sha256:")
	cache := filepath.Join(store.path, "request-images-go-v1", hash[:2], hash)
	if b, err := os.ReadFile(cache); err != nil || !bytes.Equal(b, first.Data) {
		t.Fatalf("cache missing: %v", err)
	}
	if err = os.WriteFile(cache, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := store.RequestImage(context.Background(), ref, target)
	if err != nil || !bytes.Equal(second.Data, first.Data) {
		t.Fatalf("corrupt cache was not rebuilt: %v", err)
	}
	bad := ref
	bad.Width++
	if _, err = store.RequestImage(context.Background(), bad, RequestImageTarget{Width: 20, Height: 10, MaxBytes: 1000}); err == nil {
		t.Fatal("reference metadata mismatch accepted")
	}
	budgeted, err := store.RequestImage(context.Background(), ref, RequestImageTarget{Width: 64, Height: 32, MaxBytes: 1})
	if err != nil || len(budgeted.Data) <= 1 {
		t.Fatalf("smallest exhausted-ladder fallback was not kept: %v", err)
	}
}

func TestRequestImageConcurrentCopiesAndCancellation(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.SaveImage(context.Background(), fixturePNG(t, 120, 60, false), "image/png", "concurrent.png")
	if err != nil {
		t.Fatal(err)
	}
	target := RequestImageTarget{Width: 50, Height: 25, MaxBytes: 10000}
	var group sync.WaitGroup
	results := make(chan RequestImage, 12)
	failures := make(chan error, 12)
	for range 12 {
		group.Add(1)
		go func() {
			defer group.Done()
			v, err := store.RequestImage(context.Background(), ref, target)
			if err != nil {
				failures <- err
			} else {
				results <- v
			}
		}()
	}
	group.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	var previous []byte
	for value := range results {
		if previous != nil && !bytes.Equal(previous, value.Data) {
			t.Fatal("concurrent equal transforms diverged")
		}
		previous = bytes.Clone(value.Data)
		value.Data[0] ^= 0xff
	}
	// Reserve both transform slots to make cancellation while queued observable.
	store.compression <- struct{}{}
	store.compression <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := store.RequestImage(ctx, ref, RequestImageTarget{Width: 40, Height: 20, MaxBytes: 10000})
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("cancel=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued request ignored cancellation")
	}
	<-store.compression
	<-store.compression
}

func TestRequestImageCacheSymlinkCannotWriteOutsideStore(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.SaveImage(context.Background(), fixturePNG(t, 20, 10, false), "image/png", "source.png")
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err = os.Symlink(outside, filepath.Join(store.path, "request-images-go-v1")); err != nil {
		t.Skip(err)
	}
	if _, err = store.RequestImage(context.Background(), ref, RequestImageTarget{Width: 10, Height: 5, MaxBytes: 1000}); err == nil {
		t.Fatal("symlinked cache accepted")
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) > 0 {
		t.Fatalf("outside cache mutated: %v %v", entries, err)
	}
}

// A one-pixel RGBA PNG remains alpha-channel input even if that pixel is opaque.
func TestImageAlphaChannelUsesEncodedFact(t *testing.T) {
	data := []byte{137, 80, 78, 71, 13, 10, 26, 10}
	chunk := func(kind string, content []byte) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(content)))
		data = append(data, n[:]...)
		b := append([]byte(kind), content...)
		data = append(data, b...)
		binary.BigEndian.PutUint32(n[:], crc32.ChecksumIEEE(b))
		data = append(data, n[:]...)
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[:4], 1)
	binary.BigEndian.PutUint32(ihdr[4:8], 1)
	ihdr[8], ihdr[9] = 8, 6
	chunk("IHDR", ihdr)
	raster := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	if !imageAlphaChannel(data, "png", raster) {
		t.Fatal("encoded RGBA alpha fact was discarded")
	}
}
