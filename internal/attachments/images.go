package attachments

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// Fixed defaults from attachment-local/src/index.ts at the pinned baseline.
const (
	MaxImageBytes          = 20 << 20
	MaxImagesPerMessage    = 20
	MaxMessageImageBytes   = 200 << 20
	MaxImagePixels         = 64_000_000
	MaxImageDimension      = 8192
	NormalizedMaxPixels    = 2048 * 2048
	NormalizedMaxDimension = 8192
	NormalizedMaxBytes     = 4 << 20
)

type ImageDimensions struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type ImageRef struct {
	AttachmentID       string           `json:"attachmentId"`
	MediaType          string           `json:"mediaType"`
	Bytes              int              `json:"bytes"`
	Width              int              `json:"width"`
	Height             int              `json:"height"`
	Name               string           `json:"name,omitempty"`
	OriginalDimensions *ImageDimensions `json:"originalDimensions,omitempty"`
}

type ImageInput struct {
	Data      []byte
	MediaType string
	Name      string
}

func (ref ImageRef) Source() map[string]any {
	source := map[string]any{"type": "dsgo_attachment", "id": strings.TrimPrefix(ref.AttachmentID, "sha256:"), "media_type": ref.MediaType, "bytes": ref.Bytes, "width": ref.Width, "height": ref.Height}
	if ref.Name != "" {
		source["name"] = ref.Name
	}
	if ref.OriginalDimensions != nil {
		source["original_dimensions"] = *ref.OriginalDimensions
	}
	return source
}

type ImageError struct{ Code, Message string }

func (e *ImageError) Error() string { return e.Message }

// ImageCodecStatus lets doctor disclose the one native codec dependency. PNG,
// JPEG, GIF and WebP decoding are implemented in Go; alpha conversion uses the
// installed libwebp command rather than silently dropping transparency.
func ImageCodecStatus() map[string]any {
	path, err := exec.LookPath("cwebp")
	return map[string]any{"webp_encoder": path, "webp_encoder_available": err == nil, "transparent_image_conversion_requires": "cwebp (libwebp)"}
}

func imageFailure(code, message string) error { return &ImageError{Code: code, Message: message} }

// SaveImage fully validates and normalizes before publishing a durable ref.
// Existing Put remains the byte-level compatibility API for older dsgo logs.
func (s *Store) SaveImage(ctx context.Context, data []byte, mediaType, name string) (ImageRef, error) {
	ref, prepared, err := s.prepareImage(ctx, data, mediaType, name)
	if err != nil {
		return ImageRef{}, err
	}
	return s.commitImage(ref, prepared)
}

// SaveImages refuses an invalid batch before publishing any of its images.
// This is the same preflight boundary used by official media projections: one
// image refusal must not leave some of the result's images admitted as refs.
func (s *Store) SaveImages(ctx context.Context, inputs []ImageInput) ([]ImageRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(inputs) > MaxImagesPerMessage {
		return nil, imageFailure("TOO_MANY_IMAGES", "Message exceeds the configured image-count limit.")
	}
	total := 0
	for _, input := range inputs {
		if len(input.Data) > MaxMessageImageBytes-total {
			return nil, imageFailure("MESSAGE_IMAGES_TOO_LARGE", "Message exceeds the configured aggregate image-byte limit.")
		}
		total += len(input.Data)
	}
	refs := make([]ImageRef, len(inputs))
	prepared := make([][]byte, len(inputs))
	for i, input := range inputs {
		var err error
		refs[i], prepared[i], err = s.prepareImage(ctx, input.Data, input.MediaType, input.Name)
		if err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for i := range refs {
		var err error
		refs[i], err = s.commitImage(refs[i], prepared[i])
		if err != nil {
			return nil, err
		}
	}
	return refs, nil
}

func (s *Store) commitImage(ref ImageRef, data []byte) (ImageRef, error) {
	source, err := s.Put(data, ref.MediaType)
	if err != nil {
		return ImageRef{}, err
	}
	ref.AttachmentID = "sha256:" + source["id"].(string)
	ref.Bytes = len(data)
	return ref, nil
}

func (s *Store) prepareImage(ctx context.Context, data []byte, mediaType, name string) (ImageRef, []byte, error) {
	select {
	case s.compression <- struct{}{}:
		defer func() { <-s.compression }()
	case <-ctx.Done():
		return ImageRef{}, nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return ImageRef{}, nil, err
	}
	if len(data) > MaxImageBytes {
		return ImageRef{}, nil, imageFailure("IMAGE_TOO_LARGE", "Image exceeds the configured byte limit.")
	}
	if len(data) == 0 {
		return ImageRef{}, nil, imageFailure("INVALID_IMAGE", "Image is empty.")
	}
	if !supportedImageType(mediaType) {
		return ImageRef{}, nil, imageFailure("UNSUPPORTED_IMAGE_TYPE", "Image type is not accepted by this deployment.")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return ImageRef{}, nil, imageFailure("INVALID_IMAGE", "Image bytes do not decode as PNG/JPEG/WebP/GIF.")
	}
	actual := map[string]string{"png": "image/png", "jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp"}[format]
	if config.Width < 1 || config.Height < 1 {
		return ImageRef{}, nil, imageFailure("INVALID_IMAGE", "Image dimensions are invalid.")
	}
	if config.Width > MaxImageDimension || config.Height > MaxImageDimension {
		return ImageRef{}, nil, imageFailure("IMAGE_DIMENSION_TOO_LARGE", "Image exceeds the configured per-side dimension limit.")
	}
	if int64(config.Width)*int64(config.Height) > MaxImagePixels {
		return ImageRef{}, nil, imageFailure("IMAGE_TOO_MANY_PIXELS", "Image exceeds the configured decoded pixel limit.")
	}
	if actual != mediaType {
		return ImageRef{}, nil, imageFailure("IMAGE_TYPE_MISMATCH", "Declared image type does not match its bytes.")
	}
	raster, err := decodeRaster(data, format, config)
	if err != nil {
		return ImageRef{}, nil, imageFailure("INVALID_IMAGE", "Image bytes do not fully decode.")
	}
	if err := ctx.Err(); err != nil {
		return ImageRef{}, nil, err
	}
	orientation := imageOrientation(data, format)
	raster = orientImage(raster, orientation)
	w, h := raster.Bounds().Dx(), raster.Bounds().Dy()
	nw, nh := NormalizedDimensions(w, h)
	ref := ImageRef{Width: nw, Height: nh, Name: imageDisplayName(name)}
	if nw != w || nh != h {
		ref.OriginalDimensions = &ImageDimensions{Width: w, Height: h}
	}
	prepared := data
	if nw != w || nh != h || orientation != 1 || !canPassThroughImage(data, format, raster) || len(data) > NormalizedMaxBytes {
		if nw != w || nh != h {
			resized := image.NewNRGBA(image.Rect(0, 0, nw, nh))
			xdraw.CatmullRom.Scale(resized, resized.Bounds(), raster, raster.Bounds(), draw.Src, nil)
			raster = resized
		}
		prepared, mediaType, err = encodeImageWithinBytesAlpha(ctx, raster, NormalizedMaxBytes, imageAlphaChannel(data, format, raster))
		if err != nil {
			return ImageRef{}, nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return ImageRef{}, nil, err
	}
	ref.MediaType = mediaType
	return ref, prepared, nil
}

// Native WebP's Go decoder handles still frames. The animation container is
// reduced to its first composited frame, matching the official static-image
// attachment contract without adding another decoder executable dependency.
func decodeRaster(data []byte, format string, config image.Config) (image.Image, error) {
	if format == "gif" {
		return gifFirstCanvas(data, config)
	}
	if format != "webp" || len(data) < 30 || string(data[12:16]) != "VP8X" || data[20]&2 == 0 {
		raster, _, err := image.Decode(bytes.NewReader(data))
		return raster, err
	}
	background := color.NRGBA{}
	for p := 12; p+8 <= len(data); {
		size := int(binary.LittleEndian.Uint32(data[p+4 : p+8]))
		if size > len(data)-p-8 {
			return nil, imageFailure("INVALID_IMAGE", "Animated WebP chunk is truncated.")
		}
		kind := string(data[p : p+4])
		payload := data[p+8 : p+8+size]
		if kind == "ANIM" && len(payload) >= 6 {
			background = color.NRGBA{R: payload[2], G: payload[1], B: payload[0], A: payload[3]}
		}
		if kind == "ANMF" {
			if len(payload) < 16 {
				return nil, imageFailure("INVALID_IMAGE", "Animated WebP frame is truncated.")
			}
			u24 := func(b []byte) int { return int(b[0]) | int(b[1])<<8 | int(b[2])<<16 }
			x, y, w, h := u24(payload[:3])*2, u24(payload[3:6])*2, u24(payload[6:9])+1, u24(payload[9:12])+1
			if x+w > config.Width || y+h > config.Height {
				return nil, imageFailure("INVALID_IMAGE", "Animated WebP frame exceeds its canvas.")
			}
			chunks := payload[16:]
			alpha := false
			for q := 0; q+8 <= len(chunks); {
				n := int(binary.LittleEndian.Uint32(chunks[q+4 : q+8]))
				if n > len(chunks)-q-8 {
					return nil, imageFailure("INVALID_IMAGE", "Animated WebP frame data is truncated.")
				}
				if string(chunks[q:q+4]) == "ALPH" {
					alpha = true
				}
				q += 8 + n + (n & 1)
			}
			header := make([]byte, 10)
			if alpha {
				header[0] = 16
			}
			put24 := func(b []byte, n int) { b[0] = byte(n); b[1] = byte(n >> 8); b[2] = byte(n >> 16) }
			put24(header[4:7], w-1)
			put24(header[7:10], h-1)
			body := append([]byte("WEBP"), webpChunk("VP8X", header)...)
			body = append(body, chunks...)
			still := append([]byte("RIFF"), make([]byte, 4)...)
			binary.LittleEndian.PutUint32(still[4:8], uint32(len(body)))
			still = append(still, body...)
			frame, _, err := image.Decode(bytes.NewReader(still))
			if err != nil {
				return nil, err
			}
			if frame.Bounds().Dx() != w || frame.Bounds().Dy() != h {
				return nil, imageFailure("INVALID_IMAGE", "Animated WebP frame dimensions do not match.")
			}
			canvas := image.NewNRGBA(image.Rect(0, 0, config.Width, config.Height))
			draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: background}, image.Point{}, draw.Src)
			op := draw.Over
			if payload[15]&2 != 0 {
				op = draw.Src
			}
			draw.Draw(canvas, image.Rect(x, y, x+w, y+h), frame, frame.Bounds().Min, op)
			return canvas, nil
		}
		p += 8 + size + (size & 1)
	}
	return nil, imageFailure("INVALID_IMAGE", "Animated WebP has no decodable first frame.")
}

// GIF decoders return the image descriptor's first rectangle, which may be
// smaller than and offset within the logical screen. Build just that first
// canvas; DecodeAll would unnecessarily expand every animation frame.
func gifFirstCanvas(data []byte, config image.Config) (image.Image, error) {
	frame, err := gif.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	bounds := image.Rect(0, 0, config.Width, config.Height)
	if !frame.Bounds().In(bounds) {
		return nil, imageFailure("INVALID_IMAGE", "GIF frame exceeds its logical screen.")
	}
	background := color.NRGBA{}
	if palette, ok := config.ColorModel.(color.Palette); ok && len(data) > 11 && int(data[11]) < len(palette) {
		background = color.NRGBAModel.Convert(palette[int(data[11])]).(color.NRGBA)
	}
	// Transparent GIFs are rendered over a transparent application canvas;
	// the transparent palette entry must not be flattened into the GCT color.
	if palette, ok := frame.ColorModel().(color.Palette); ok {
		for _, entry := range palette {
			if _, _, _, alpha := entry.RGBA(); alpha == 0 {
				background = color.NRGBA{}
				break
			}
		}
	}
	canvas := image.NewNRGBA(bounds)
	if background.A != 0 {
		draw.Draw(canvas, bounds, image.NewUniform(background), image.Point{}, draw.Src)
	}
	draw.Draw(canvas, frame.Bounds(), frame, frame.Bounds().Min, draw.Over)
	return canvas, nil
}

func webpChunk(kind string, payload []byte) []byte {
	chunk := make([]byte, 8, len(payload)+9)
	copy(chunk, kind)
	binary.LittleEndian.PutUint32(chunk[4:8], uint32(len(payload)))
	chunk = append(chunk, payload...)
	if len(payload)&1 != 0 {
		chunk = append(chunk, 0)
	}
	return chunk
}

// NormalizedDimensions follows official requestImageDimensions geometry and
// then the independent long-edge cap. It never enlarges a source raster.
func NormalizedDimensions(width, height int) (int, int) {
	scale := math.Min(1, math.Sqrt(float64(NormalizedMaxPixels)/(float64(width)*float64(height))))
	w, h := width, height
	if scale < 1 {
		if width >= height {
			w = max(1, int(math.Floor(float64(width)*scale)))
			h = max(1, int(math.Floor(float64(w)*float64(height)/float64(width)+0.5)))
			for w*h > NormalizedMaxPixels && w > 1 {
				w--
				h = max(1, int(math.Floor(float64(w)*float64(height)/float64(width)+0.5)))
			}
		} else {
			h = max(1, int(math.Floor(float64(height)*scale)))
			w = max(1, int(math.Floor(float64(h)*float64(width)/float64(height)+0.5)))
			for w*h > NormalizedMaxPixels && h > 1 {
				h--
				w = max(1, int(math.Floor(float64(h)*float64(width)/float64(height)+0.5)))
			}
		}
	}
	if max(w, h) > NormalizedMaxDimension {
		if w >= h {
			h = max(1, int(math.Floor(float64(NormalizedMaxDimension)*float64(h)/float64(w)+0.5)))
			w = NormalizedMaxDimension
		} else {
			w = max(1, int(math.Floor(float64(NormalizedMaxDimension)*float64(w)/float64(h)+0.5)))
			h = NormalizedMaxDimension
		}
	}
	return w, h
}

func encodeNormalized(ctx context.Context, raster image.Image) ([]byte, string, error) {
	return encodeImageWithinBytes(ctx, raster, NormalizedMaxBytes)
}

// Normalization and provider-request variants use the same official lazy
// quality ladder. The byte budget is a target: if no quality fits, preserve the
// smallest result instead of repeatedly shrinking the chosen raster geometry.
func encodeImageWithinBytes(ctx context.Context, raster image.Image, maxBytes int) ([]byte, string, error) {
	return encodeImageWithinBytesAlpha(ctx, raster, maxBytes, hasTransparentPixels(raster))
}

func hasTransparentPixels(raster image.Image) bool {
	alpha := false
	if opaque, ok := raster.(interface{ Opaque() bool }); ok {
		alpha = !opaque.Opaque()
	} else {
		bounds := raster.Bounds()
		for y := bounds.Min.Y; y < bounds.Max.Y && !alpha; y++ {
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				_, _, _, a := raster.At(x, y).RGBA()
				if a != 65535 {
					alpha = true
					break
				}
			}
		}
	}
	return alpha
}

// Codec selection follows the source alpha-channel fact. An RGBA PNG whose
// pixels happen to be opaque still selects WebP when a transform is required.
func imageAlphaChannel(data []byte, format string, raster image.Image) bool {
	switch format {
	case "jpeg":
		return false
	case "png":
		if len(data) > 25 && (data[25] == 4 || data[25] == 6) {
			return true
		}
		for p := 8; p+12 <= len(data); {
			size := int(binary.BigEndian.Uint32(data[p : p+4]))
			if size > len(data)-p-12 {
				return hasTransparentPixels(raster)
			}
			if string(data[p+4:p+8]) == "tRNS" {
				return true
			}
			p += size + 12
		}
	case "webp":
		for p := 12; p+8 <= len(data); {
			size := int(binary.LittleEndian.Uint32(data[p+4 : p+8]))
			if size > len(data)-p-8 {
				return hasTransparentPixels(raster)
			}
			kind := string(data[p : p+4])
			payload := data[p+8 : p+8+size]
			if kind == "VP8X" && len(payload) > 0 {
				return payload[0]&0x10 != 0
			}
			if kind == "VP8L" && len(payload) >= 5 {
				return binary.LittleEndian.Uint32(payload[1:5])&(1<<28) != 0
			}
			p += 8 + size + (size & 1)
		}
	}
	return hasTransparentPixels(raster)
}

func encodeImageWithinBytesAlpha(ctx context.Context, raster image.Image, maxBytes int, alpha bool) ([]byte, string, error) {
	var smallest []byte
	mediaType := "image/jpeg"
	var pngBytes bytes.Buffer
	encoder := ""
	if alpha {
		mediaType = "image/webp"
		var err error
		encoder, err = exec.LookPath("cwebp")
		if err != nil {
			return nil, "", imageFailure("IMAGE_CODEC_UNAVAILABLE", "Transparent image normalization requires the cwebp encoder (libwebp); install it and retry.")
		}
		if err = png.Encode(&pngBytes, raster); err != nil {
			return nil, "", err
		}
	}
	for _, quality := range []int{85, 75, 60} {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		var candidate bytes.Buffer
		if alpha {
			command := exec.CommandContext(ctx, encoder, "-quiet", "-q", strconv.Itoa(quality), "-m", "0", "-metadata", "none", "-o", "-", "--", "-")
			command.Stdin = bytes.NewReader(pngBytes.Bytes())
			command.Stdout = &candidate
			if err := command.Run(); err != nil {
				if ctx.Err() != nil {
					return nil, "", ctx.Err()
				}
				return nil, "", imageFailure("IMAGE_CODEC_FAILED", "Unable to encode a normalized WebP image.")
			}
		} else if err := jpeg.Encode(&candidate, raster, &jpeg.Options{Quality: quality}); err != nil {
			return nil, "", err
		}
		encoded := candidate.Bytes()
		if smallest == nil || len(encoded) < len(smallest) {
			smallest = append([]byte(nil), encoded...)
		}
		if len(encoded) <= maxBytes {
			return append([]byte(nil), encoded...), mediaType, nil
		}
	}
	return smallest, mediaType, nil
}

func imageDisplayName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	units := utf16.Encode([]rune(name))
	if len(units) > 255 {
		name = string(utf16.Decode(units[:255]))
	}
	if name == "." {
		return ""
	}
	return name
}

func canPassThroughImage(data []byte, format string, raster image.Image) bool {
	switch format {
	case "png":
		if len(data) < 29 || data[24] != 8 {
			return false
		}
		for p := 8; p+12 <= len(data); {
			size := int(binary.BigEndian.Uint32(data[p : p+4]))
			if size > len(data)-p-12 {
				return false
			}
			kind := string(data[p+4 : p+8])
			switch kind {
			case "IHDR", "IDAT", "IEND", "PLTE", "tRNS", "sRGB":
			default:
				return false
			}
			p += size + 12
		}
		return true
	case "jpeg":
		if _, ok := raster.(*image.YCbCr); !ok {
			return false
		}
		for p := 2; p+4 <= len(data); {
			if data[p] != 0xff {
				return false
			}
			marker := data[p+1]
			p += 2
			if marker == 0xda || marker == 0xd9 {
				return true
			}
			if marker == 0x01 || marker >= 0xd0 && marker <= 0xd7 {
				continue
			}
			size := int(binary.BigEndian.Uint16(data[p : p+2]))
			if size < 2 || size > len(data)-p {
				return false
			}
			if marker >= 0xe1 && marker <= 0xef || marker == 0xfe {
				return false
			}
			p += size
		}
	case "webp":
		for p := 12; p+8 <= len(data); {
			size := int(binary.LittleEndian.Uint32(data[p+4 : p+8]))
			if size > len(data)-p-8 {
				return false
			}
			switch string(data[p : p+4]) {
			case "VP8 ", "VP8L", "VP8X", "ALPH":
			default:
				return false
			}
			p += 8 + size + (size & 1)
		}
		return true
	}
	return false
}

func jpegOrientation(data []byte) int {
	for p := 2; p+4 <= len(data); {
		if data[p] != 0xff {
			return 1
		}
		marker := data[p+1]
		p += 2
		if marker == 0xda || marker == 0xd9 {
			return 1
		}
		if marker == 0x01 || marker >= 0xd0 && marker <= 0xd7 {
			continue
		}
		size := int(binary.BigEndian.Uint16(data[p : p+2]))
		if size < 2 || size > len(data)-p {
			return 1
		}
		if marker == 0xe1 && size >= 16 && bytes.Equal(data[p+2:p+8], []byte("Exif\x00\x00")) {
			return tiffOrientation(data[p+8 : p+size])
		}
		p += size
	}
	return 1
}

func imageOrientation(data []byte, format string) int {
	if format == "jpeg" {
		return jpegOrientation(data)
	}
	start := 8
	if format == "webp" {
		start = 12
	}
	if format != "png" && format != "webp" {
		return 1
	}
	for p := start; p+8 <= len(data); {
		size := 0
		kind := ""
		payloadStart := p + 8
		overhead := 8
		if format == "png" {
			size = int(binary.BigEndian.Uint32(data[p : p+4]))
			kind = string(data[p+4 : p+8])
			overhead = 12
		} else {
			size = int(binary.LittleEndian.Uint32(data[p+4 : p+8]))
			kind = string(data[p : p+4])
		}
		if size > len(data)-p-overhead {
			return 1
		}
		if kind == "eXIf" || kind == "EXIF" {
			payload := data[payloadStart : payloadStart+size]
			if bytes.HasPrefix(payload, []byte("Exif\x00\x00")) {
				payload = payload[6:]
			}
			return tiffOrientation(payload)
		}
		p += size + overhead
		if format == "webp" {
			p += size & 1
		}
	}
	return 1
}

func tiffOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var order binary.ByteOrder
	if bytes.Equal(tiff[:2], []byte("II")) {
		order = binary.LittleEndian
	} else if bytes.Equal(tiff[:2], []byte("MM")) {
		order = binary.BigEndian
	} else {
		return 1
	}
	if order.Uint16(tiff[2:4]) != 42 {
		return 1
	}
	offset := int(order.Uint32(tiff[4:8]))
	if offset < 8 || offset+2 > len(tiff) {
		return 1
	}
	count := int(order.Uint16(tiff[offset : offset+2]))
	for i := 0; i < count; i++ {
		entry := offset + 2 + i*12
		if entry+12 > len(tiff) {
			return 1
		}
		if order.Uint16(tiff[entry:entry+2]) == 0x112 && order.Uint16(tiff[entry+2:entry+4]) == 3 && order.Uint32(tiff[entry+4:entry+8]) == 1 {
			orientation := int(order.Uint16(tiff[entry+8 : entry+10]))
			if orientation >= 1 && orientation <= 8 {
				return orientation
			}
			return 1
		}
	}
	return 1
}

func orientImage(source image.Image, orientation int) image.Image {
	if orientation == 1 {
		return source
	}
	bounds := source.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	nw, nh := w, h
	if orientation >= 5 {
		nw, nh = h, w
	}
	out := image.NewNRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dx, dy := x, y
			switch orientation {
			case 2:
				dx = w - 1 - x
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dy = h - 1 - y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			out.Set(dx, dy, color.NRGBAModel.Convert(source.At(bounds.Min.X+x, bounds.Min.Y+y)))
		}
	}
	return out
}
