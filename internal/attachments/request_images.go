package attachments

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/draw"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	xdraw "golang.org/x/image/draw"
)

// RequestImageTransformVersion follows the pinned attachment-local transform.
// Pixel/codec implementation identity is additionally separated on disk: Go's
// encoder is not byte-identical to the upstream Sharp/libvips pipeline.
const RequestImageTransformVersion = "request-image-v6"

type RequestImageTarget struct {
	Width    int `json:"width"`
	Height   int `json:"height"`
	MaxBytes int `json:"maxBytes"`
}

type RequestImage struct {
	VariantID  string   `json:"variantId"`
	Attachment ImageRef `json:"attachment"`
	Data       []byte   `json:"-"`
	MediaType  string   `json:"mediaType"`
	Bytes      int      `json:"bytes"`
	Width      int      `json:"width"`
	Height     int      `json:"height"`
	HasAlpha   bool     `json:"hasAlpha"`
}

func validateImageTarget(target RequestImageTarget) error {
	const maxSafeInteger = 9007199254740991
	for _, n := range []int{target.Width, target.Height, target.MaxBytes} {
		if n <= 0 || int64(n) > maxSafeInteger {
			return imageFailure("INVALID_ATTACHMENT_REF", "Image request dimensions and byte target must be positive safe integers.")
		}
	}
	return nil
}

// RequestImageVariantID pins every upstream transform input. Never use model
// names as a substitute for the route's resolved dimensions and byte budget.
func RequestImageVariantID(ref ImageRef, target RequestImageTarget) (string, error) {
	if err := validateImageTarget(target); err != nil {
		return "", err
	}
	if !strings.HasPrefix(ref.AttachmentID, "sha256:") || !validID.MatchString(strings.TrimPrefix(ref.AttachmentID, "sha256:")) {
		return "", imageFailure("INVALID_ATTACHMENT_REF", "Image attachment identity is invalid.")
	}
	descriptor := struct {
		TransformVersion  string `json:"transformVersion"`
		AttachmentID      string `json:"attachmentId"`
		TargetWidth       int    `json:"targetWidth"`
		TargetHeight      int    `json:"targetHeight"`
		EncodedByteBudget int    `json:"encodedByteBudget"`
		Encoding          struct {
			WebpQualities []int    `json:"webpQualities"`
			WebpEffort    int      `json:"webpEffort"`
			JpegQualities []int    `json:"jpegQualities"`
			Order         []string `json:"order"`
			Colourspace   string   `json:"colourspace"`
		} `json:"encoding"`
	}{TransformVersion: RequestImageTransformVersion, AttachmentID: ref.AttachmentID, TargetWidth: target.Width, TargetHeight: target.Height, EncodedByteBudget: target.MaxBytes}
	descriptor.Encoding.WebpQualities = []int{85, 75, 60}
	descriptor.Encoding.WebpEffort = 0
	descriptor.Encoding.JpegQualities = []int{85, 75, 60}
	descriptor.Encoding.Order = []string{"alpha:webp", "opaque:jpeg"}
	descriptor.Encoding.Colourspace = "srgb"
	data, err := json.Marshal(descriptor)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

type imageFlight struct {
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	waiters  int
	finished bool
	result   RequestImage
	err      error
}

var imageFlights = struct {
	sync.Mutex
	byKey map[string]*imageFlight
}{byKey: map[string]*imageFlight{}}

// RequestImage returns a verified route-chosen image without changing the
// durable provider-independent attachment. Equal concurrent requests share one
// operation; one cancelled waiter cannot abort another waiter's transformation.
func (s *Store) RequestImage(ctx context.Context, ref ImageRef, target RequestImageTarget) (RequestImage, error) {
	if err := ctx.Err(); err != nil {
		return RequestImage{}, err
	}
	variant, err := RequestImageVariantID(ref, target)
	if err != nil {
		return RequestImage{}, err
	}
	key := s.path + "\x00" + variant
	imageFlights.Lock()
	flight := imageFlights.byKey[key]
	if flight != nil && flight.ctx.Err() != nil {
		flight = nil
	}
	if flight == nil {
		workCtx, cancel := context.WithCancel(context.Background())
		flight = &imageFlight{ctx: workCtx, cancel: cancel, done: make(chan struct{})}
		imageFlights.byKey[key] = flight
		go func(f *imageFlight) {
			value, err := s.requestImage(f.ctx, ref, target, variant)
			imageFlights.Lock()
			f.result, f.err, f.finished = value, err, true
			if imageFlights.byKey[key] == f {
				delete(imageFlights.byKey, key)
			}
			close(f.done)
			f.cancel()
			imageFlights.Unlock()
		}(flight)
	}
	flight.waiters++
	imageFlights.Unlock()
	release := func(cancelled bool) {
		imageFlights.Lock()
		flight.waiters--
		if cancelled && flight.waiters == 0 && !flight.finished {
			flight.cancel()
		}
		imageFlights.Unlock()
	}
	select {
	case <-ctx.Done():
		release(true)
		return RequestImage{}, ctx.Err()
	case <-flight.done:
		release(false)
		value := flight.result
		value.Data = bytes.Clone(value.Data)
		return value, flight.err
	}
}

func (s *Store) requestImage(ctx context.Context, ref ImageRef, target RequestImageTarget, variant string) (RequestImage, error) {
	select {
	case s.compression <- struct{}{}:
		defer func() { <-s.compression }()
	case <-ctx.Done():
		return RequestImage{}, ctx.Err()
	}
	data, err := s.Read(strings.TrimPrefix(ref.AttachmentID, "sha256:"))
	if err != nil {
		return RequestImage{}, err
	}
	if err = ctx.Err(); err != nil {
		return RequestImage{}, err
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	media := map[string]string{"png": "image/png", "jpeg": "image/jpeg", "webp": "image/webp", "gif": "image/gif"}[format]
	if err != nil || config.Width != ref.Width || config.Height != ref.Height || media != ref.MediaType || config.Width < 1 || config.Height < 1 || config.Width > MaxImageDimension || config.Height > MaxImageDimension || int64(config.Width)*int64(config.Height) > MaxImagePixels {
		return RequestImage{}, imageFailure("INVALID_ATTACHMENT_REF", "Stored image does not match its durable reference.")
	}
	raster, err := decodeRaster(data, format, config)
	if err != nil {
		return RequestImage{}, imageFailure("INVALID_ATTACHMENT_REF", "Stored image does not fully decode.")
	}
	alpha := imageAlphaChannel(data, format, raster)
	cachePath := filepath.Join("request-images-go-v1", strings.TrimPrefix(variant, "sha256:")[:2], strings.TrimPrefix(variant, "sha256:"))
	if cached, ok := s.cachedRequestImage(ctx, cachePath, ref, target, variant, alpha); ok {
		return cached, nil
	}
	if err = ctx.Err(); err != nil {
		return RequestImage{}, err
	}
	if target.Width >= ref.Width && target.Height >= ref.Height && len(data) <= target.MaxBytes && canPassThroughImage(data, format, raster) {
		return RequestImage{VariantID: variant, Attachment: ref, Data: data, MediaType: ref.MediaType, Bytes: len(data), Width: ref.Width, Height: ref.Height, HasAlpha: alpha}, nil
	}
	w, h := ref.Width, ref.Height
	if w >= h {
		w = min(w, target.Width)
		h = max(1, int(math.Floor(float64(ref.Height)*float64(w)/float64(ref.Width)+0.5)))
	} else {
		h = min(h, target.Height)
		w = max(1, int(math.Floor(float64(ref.Width)*float64(h)/float64(ref.Height)+0.5)))
	}
	if w != ref.Width || h != ref.Height {
		scaled := image.NewNRGBA(image.Rect(0, 0, w, h))
		xdraw.CatmullRom.Scale(scaled, scaled.Bounds(), raster, raster.Bounds(), draw.Src, nil)
		raster = scaled
	}
	encoded, encodedMedia, err := encodeImageWithinBytesAlpha(ctx, raster, target.MaxBytes, alpha)
	if err != nil {
		return RequestImage{}, err
	}
	verified, err := verifiedRequestImage(encoded, ref, target, variant, alpha, false)
	if err != nil {
		return RequestImage{}, err
	}
	if verified.Width != w || verified.Height != h || verified.MediaType != encodedMedia {
		return RequestImage{}, imageFailure("ATTACHMENT_WRITE_FAILED", "Encoded model-request image facts do not match.")
	}
	if err = ctx.Err(); err != nil {
		return RequestImage{}, err
	}
	if err = s.writeRequestImage(cachePath, encoded); err != nil {
		return RequestImage{}, err
	}
	return verified, nil
}

func verifiedRequestImage(data []byte, ref ImageRef, target RequestImageTarget, variant string, expectedAlpha, checkBounds bool) (RequestImage, error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	media := map[string]string{"png": "image/png", "jpeg": "image/jpeg", "webp": "image/webp"}[format]
	if err != nil || media == "" || config.Width < 1 || config.Height < 1 || config.Width > MaxImageDimension || config.Height > MaxImageDimension || int64(config.Width)*int64(config.Height) > MaxImagePixels {
		return RequestImage{}, imageFailure("ATTACHMENT_WRITE_FAILED", "Invalid model-request image bytes.")
	}
	if checkBounds && (config.Width > target.Width || config.Height > target.Height) {
		return RequestImage{}, imageFailure("ATTACHMENT_WRITE_FAILED", "Cached image exceeds the route target.")
	}
	raster, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return RequestImage{}, err
	}
	alpha := imageAlphaChannel(data, format, raster)
	if alpha != expectedAlpha && !(expectedAlpha && !alpha && media == "image/webp") {
		return RequestImage{}, imageFailure("ATTACHMENT_WRITE_FAILED", "Model-request image changed its alpha representation.")
	}
	if format == "png" && (len(data) < 25 || data[24] != 8) {
		return RequestImage{}, imageFailure("ATTACHMENT_WRITE_FAILED", "Model-request PNG must have 8-bit channels.")
	}
	return RequestImage{VariantID: variant, Attachment: ref, Data: data, MediaType: media, Bytes: len(data), Width: config.Width, Height: config.Height, HasAlpha: alpha}, nil
}

func (s *Store) cachedRequestImage(ctx context.Context, path string, ref ImageRef, target RequestImageTarget, variant string, alpha bool) (RequestImage, bool) {
	root, err := os.OpenRoot(s.path)
	if err != nil {
		return RequestImage{}, false
	}
	defer root.Close()
	info, err := root.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxBytes {
		return RequestImage{}, false
	}
	f, err := root.Open(path)
	if err != nil {
		return RequestImage{}, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil || len(data) > maxBytes || ctx.Err() != nil {
		return RequestImage{}, false
	}
	image, err := verifiedRequestImage(data, ref, target, variant, alpha, true)
	return image, err == nil
}

func (s *Store) writeRequestImage(path string, data []byte) error {
	root, err := os.OpenRoot(s.path)
	if err != nil {
		return err
	}
	defer root.Close()
	parent := filepath.Dir(path)
	if err = root.MkdirAll(parent, 0700); err != nil {
		return err
	}
	for current := parent; current != "."; current = filepath.Dir(current) {
		info, err := root.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("request-image cache must use real directories")
		}
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	temporary := path + "." + hex.EncodeToString(nonce[:]) + ".tmp"
	f, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temporary)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = root.Rename(temporary, path); err != nil {
		return err
	}
	dir, err := root.Open(parent)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
