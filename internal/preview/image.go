package preview

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"os"
	"strings"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

const maxPixels = 16_777_216

func imagePreview(data []byte, zoom int) (*ImageInfo, []byte, error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, nil, fmt.Errorf("unsupported or invalid image: %w", err)
	}
	if config.Width < 1 || config.Height < 1 || int64(config.Width)*int64(config.Height) > maxPixels {
		return nil, nil, errors.New("image exceeds 16,777,216 pixels")
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, nil, err
	}
	w, h := max(1, config.Width*zoom/100), max(1, config.Height*zoom/100)
	if int64(w)*int64(h) > maxPixels {
		return nil, nil, errors.New("zoomed image exceeds 16,777,216 pixels")
	}
	if w != config.Width || h != config.Height {
		scaled := image.NewRGBA(image.Rect(0, 0, w, h))
		bounds := img.Bounds()
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				scaled.Set(x, y, img.At(bounds.Min.X+x*config.Width/w, bounds.Min.Y+y*config.Height/h))
			}
		}
		img = scaled
	} else {
		clean := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(clean, clean.Bounds(), img, img.Bounds().Min, draw.Src)
		img = clean
	}
	var out bytes.Buffer
	if err = png.Encode(&out, img); err != nil {
		return nil, nil, err
	}
	return &ImageInfo{Format: format, Width: config.Width, Height: config.Height, Zoom: zoom}, out.Bytes(), nil
}

// TerminalProtocol is conservative: redirected output always uses metadata.
func TerminalProtocol(w io.Writer, getenv func(string) string) string {
	f, ok := w.(*os.File)
	if !ok {
		return "none"
	}
	if !isTTY(f) {
		return "none"
	}
	if getenv("TMUX") != "" || getenv("STY") != "" {
		return "none"
	}
	if strings.Contains(getenv("TERM"), "kitty") && getenv("KITTY_WINDOW_ID") != "" {
		return "kitty"
	}
	if getenv("TERM_PROGRAM") == "iTerm.app" {
		return "iterm2"
	}
	return "none"
}

// EncodeImage writes only a validated, re-encoded PNG. Callers must gate this on
// a real terminal; arbitrary document text cannot introduce escape sequences.
func EncodeImage(pngData []byte, protocol string) ([]byte, error) {
	if len(pngData) > MaxBytes {
		return nil, errors.New("image transfer exceeds size limit")
	}
	if config, format, err := image.DecodeConfig(bytes.NewReader(pngData)); err != nil || format != "png" || config.Width < 1 || config.Height < 1 || int64(config.Width)*int64(config.Height) > maxPixels {
		return nil, errors.New("image protocol requires a valid PNG")
	}
	encoded := base64.StdEncoding.EncodeToString(pngData)
	var out strings.Builder
	switch protocol {
	case "kitty":
		for offset := 0; offset < len(encoded); offset += 4096 {
			end := min(len(encoded), offset+4096)
			more := 0
			if end < len(encoded) {
				more = 1
			}
			if offset == 0 {
				fmt.Fprintf(&out, "\x1b_Ga=T,f=100,t=d,q=2,m=%d;", more)
			} else {
				fmt.Fprintf(&out, "\x1b_Gm=%d;", more)
			}
			out.WriteString(encoded[offset:end])
			out.WriteString("\x1b\\")
		}
	case "iterm2":
		fmt.Fprintf(&out, "\x1b]1337;File=inline=1;size=%d;width=auto;height=auto;preserveAspectRatio=1:%s\a", len(pngData), encoded)
	default:
		return nil, errors.New("inline images require a detected Kitty or iTerm2 terminal")
	}
	return []byte(out.String()), nil
}
