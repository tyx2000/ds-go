package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"dsgo/internal/attachments"
	"dsgo/internal/core"
	"dsgo/internal/model"
)

func imageToolSpec() core.ToolSpec {
	return toolSpec("read_image", "Read a PNG/JPEG/WebP/GIF file and return the image itself. Large images are downscaled automatically; do not install image libraries or create thumbnails to inspect an image.", `{"type":"object","additionalProperties":false,"properties":{"file_path":{"type":"string","description":"Path to the image file, resolved by the filesystem backend."}},"required":["file_path"]}`)
}

func (a *App) registerImageTool() error {
	if a.attachmentStore == nil {
		return nil
	}
	return a.registry.Register(imageToolSpec(), a.readImage)
}

func imageExtension(path string) string {
	base := filepath.Base(path)
	if base == "." || base == ".." || strings.HasPrefix(base, ".") && !strings.Contains(base[1:], ".") {
		return ""
	}
	return strings.ToLower(filepath.Ext(path))
}

func declaredImageType(path string) string {
	return map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp", ".gif": "image/gif"}[imageExtension(path)]
}

func sniffImageType(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}):
		return "image/png"
	case bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif"
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp"
	default:
		return ""
	}
}

func (a *App) assertImageRoute(ctx context.Context, sessionID, requestedPath string) error {
	if a.store == nil || sessionID == "" {
		return fmt.Errorf("cannot read %q as an image: the current model route could not be resolved", requestedPath)
	}
	session, err := a.store.Get(sessionID)
	if err != nil {
		return err
	}
	provider, name := session.Provider, session.Model
	if provider == "" {
		provider = a.cfg.SelectedProvider
	}
	if provider == "" {
		provider = "deepseek-official"
	}
	if name == "" {
		name = a.cfg.Model
	}
	if name == "" {
		return fmt.Errorf("cannot read %q as an image: the current model route could not be resolved", requestedPath)
	}
	var catalog []model.ModelInfo
	if provider == "deepseek-official" && officialAPI(a.cfg.BaseURL) {
		_, token, err := credentials(a.cfg)
		if err != nil {
			return err
		}
		if token != "" {
			catalog, err = model.New(model.Options{BaseURL: a.cfg.BaseURL, Token: token}).ListAccountModels(ctx)
			if err != nil {
				return err
			}
		} else {
			catalog = model.BuiltinModels()
		}
	} else if raw, ok := a.cfg.ProviderProfiles[provider]; ok {
		var options model.ProviderOptions
		if err = json.Unmarshal(raw, &options); err != nil {
			return err
		}
		client, err := model.NewProvider(options)
		if err != nil {
			return err
		}
		catalog, err = client.ListModels(ctx)
		if err != nil {
			return err
		}
	}
	for _, info := range catalog {
		if info.ID == name && info.Supported {
			for _, modality := range info.InputModalities {
				if modality == "image" {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("cannot read %q as an image: model %q does not declare image input; switch to an image-capable model to read images", requestedPath, name)
}

func (a *App) readImage(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	var input struct {
		FilePath string `json:"file_path"`
	}
	if err := decodeOfficialObject(raw, &input); err != nil {
		return core.ToolResult{}, err
	}
	if strings.TrimFunc(input.FilePath, jsWhitespace) == "" {
		return core.ToolResult{}, errors.New("file_path must be a non-empty string")
	}
	extension := imageExtension(input.FilePath)
	declared := declaredImageType(input.FilePath)
	if declared == "" && extension != "" {
		return core.ToolResult{}, fmt.Errorf("cannot read %q: the %s extension does not declare a supported image format; read_image accepts PNG/JPEG/WebP/GIF files, including extension-less files in those formats", input.FilePath, extension)
	}
	if a.attachmentStore == nil {
		return core.ToolResult{}, fmt.Errorf("cannot read %q as an image: no attachment service is mounted", input.FilePath)
	}
	if err := a.assertImageRoute(ctx, tc.SessionID, input.FilePath); err != nil {
		return core.ToolResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return core.ToolResult{}, err
	}
	path := input.FilePath
	if !filepath.IsAbs(path) {
		path = filepath.Join(tc.CWD, path)
	}
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return core.ToolResult{}, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return core.ToolResult{}, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return core.ToolResult{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return core.ToolResult{}, err
	}
	if !info.Mode().IsRegular() {
		return core.ToolResult{}, errors.New("image path must refer to a regular file")
	}
	if info.Size() > attachments.MaxImageBytes {
		return core.ToolResult{}, fmt.Errorf("cannot read %q: the image cannot be stored within the deployment's byte limits; downscale the image and read the smaller copy", path)
	}
	data, err := io.ReadAll(io.LimitReader(imageContextReader{ctx: ctx, reader: file}, attachments.MaxImageBytes+1))
	if err != nil {
		return core.ToolResult{}, err
	}
	mediaType := declared
	if mediaType == "" {
		mediaType = sniffImageType(data)
	}
	if mediaType == "" {
		return core.ToolResult{}, fmt.Errorf("cannot read %q: the file content is not a supported image format; read_image accepts PNG/JPEG/WebP/GIF", path)
	}
	ref, err := a.attachmentStore.SaveImage(ctx, data, mediaType, filepath.Base(path))
	if err != nil {
		var imageErr *attachments.ImageError
		if !errors.As(err, &imageErr) {
			return core.ToolResult{}, err
		}
		switch imageErr.Code {
		case "IMAGE_DIMENSION_TOO_LARGE":
			return core.ToolResult{}, fmt.Errorf("cannot read %q: at least one image side exceeds the %dpx limit; downscale the image and read the smaller copy", path, attachments.MaxImageDimension)
		case "IMAGE_TOO_MANY_PIXELS":
			return core.ToolResult{}, fmt.Errorf("cannot read %q: the image exceeds the %d-pixel decoded-size limit; downscale the image and read the smaller copy", path, attachments.MaxImagePixels)
		case "IMAGE_TOO_LARGE":
			return core.ToolResult{}, fmt.Errorf("cannot read %q: the image cannot be stored within the deployment's byte limits; downscale the image and read the smaller copy", path)
		case "INVALID_IMAGE":
			if declared == "" {
				return core.ToolResult{}, fmt.Errorf("cannot read %q: the bytes do not decode as a supported PNG/JPEG/WebP/GIF image; the file may be truncated or corrupt", path)
			}
		case "IMAGE_TYPE_MISMATCH":
			if declared == "" {
				return core.ToolResult{}, fmt.Errorf("cannot read %q: the file signature claims %s, but the bytes decode as a different image format; the file may be corrupt", path, mediaType)
			}
			return core.ToolResult{}, fmt.Errorf("cannot read %q: the %s extension declares %s, but the bytes use a different image format; rename the file to match its actual format if it is PNG/JPEG/WebP/GIF, or convert it to one of those formats", path, extension, mediaType)
		}
		return core.ToolResult{}, err
	}
	text := formatImageReadOutput(path, ref)
	return core.ToolResult{Content: text, Blocks: []core.Block{{Type: "text", Text: text}, {Type: "image", Source: ref.Source()}}}, nil
}

func formatImageReadOutput(path string, ref attachments.ImageRef) string {
	scaled := ""
	if ref.OriginalDimensions != nil {
		x := fmt.Sprintf("%.2f", float64(ref.OriginalDimensions.Width)/float64(ref.Width))
		y := fmt.Sprintf("%.2f", float64(ref.OriginalDimensions.Height)/float64(ref.Height))
		advice := "multiply coordinates by " + x
		if x != y {
			advice = "multiply x coordinates by " + x + " and y coordinates by " + y
		}
		scaled = fmt.Sprintf(" (downscaled from %dx%d px; %s to locate features in the original file)", ref.OriginalDimensions.Width, ref.OriginalDimensions.Height, advice)
	}
	return fmt.Sprintf("<path>%s</path>\n<type>image</type>\n<content>\n%s image, %dx%d px, %d bytes%s\n</content>", path, ref.MediaType, ref.Width, ref.Height, ref.Bytes, scaled)
}

type imageContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r imageContextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(data)
}
