package attachments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strings"

	"dsgo/internal/core"
)

// ImageProjection describes the exact representation shared by request
// serialization and route pricing. A nil Target preserves original bytes;
// Omit emits only Text for a text-only route.
type ImageProjection struct {
	Target *RequestImageTarget
	Text   string
	Omit   bool
}
type ImageProjector func(context.Context, core.Request, ImageRef, bool) (ImageProjection, error)

// ImageReference reads durable metadata without modifying it. Old dsgo refs
// without dimensions are resolved from their verified stored bytes. Inline
// compatibility blocks retain their original representation and have no local
// durable access path.
func (s *Store) ImageReference(ctx context.Context, block core.Block) (ImageRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return ImageRef{}, false, err
	}
	var source struct {
		Type, ID, Data       string
		MediaType            string `json:"media_type"`
		Width, Height, Bytes int
		Name                 string
	}
	if err := json.Unmarshal(core.JSON(block.Source), &source); err != nil {
		return ImageRef{}, false, err
	}
	ref := ImageRef{AttachmentID: "sha256:" + source.ID, MediaType: source.MediaType, Width: source.Width, Height: source.Height, Bytes: source.Bytes, Name: source.Name}
	durable := source.Type == "dsgo_attachment"
	if durable && !validID.MatchString(source.ID) {
		return ref, true, errors.New("invalid image attachment identity")
	}
	if !durable && source.Type != "base64" {
		return ref, false, errors.New("image pricing requires a durable or inline image source")
	}
	if !supportedImageType(source.MediaType) {
		return ref, durable, errors.New("invalid image attachment media type")
	}
	if durable && ref.Width > 0 && ref.Height > 0 && ref.Bytes > 0 {
		if ref.Width > MaxImageDimension || ref.Height > MaxImageDimension || int64(ref.Width)*int64(ref.Height) > MaxImagePixels || ref.Bytes > maxBytes {
			return ref, true, errors.New("invalid image attachment dimensions or byte size")
		}
		return ref, true, nil
	}
	var data []byte
	var err error
	if durable {
		if s == nil {
			return ref, true, errors.New("image attachment service unavailable")
		}
		data, err = s.Read(source.ID)
	} else {
		if len(source.Data) > base64.StdEncoding.EncodedLen(maxBytes) {
			return ref, false, errors.New("inline image exceeds byte limit")
		}
		data, err = base64.StdEncoding.DecodeString(source.Data)
	}
	if err != nil {
		return ref, durable, err
	}
	if !durable {
		digest := sha256.Sum256(data)
		ref.AttachmentID = "sha256:" + hex.EncodeToString(digest[:])
	}
	facts, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || map[string]string{"png": "image/png", "jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp"}[format] != ref.MediaType {
		return ref, durable, errors.New("image source does not match its media type")
	}
	ref.Width, ref.Height, ref.Bytes = facts.Width, facts.Height, len(data)
	if ref.Width < 1 || ref.Height < 1 || ref.Width > MaxImageDimension || ref.Height > MaxImageDimension || int64(ref.Width)*int64(ref.Height) > MaxImagePixels {
		return ref, durable, errors.New("invalid image dimensions")
	}
	return ref, durable, ctx.Err()
}

func (s *Store) ImageHostPath(ref ImageRef) (string, error) {
	if s == nil {
		return "", errors.New("image attachment service unavailable")
	}
	id := strings.TrimPrefix(ref.AttachmentID, "sha256:")
	if !validID.MatchString(id) {
		return "", errors.New("invalid image attachment identity")
	}
	root, err := os.OpenRoot(s.path)
	if err != nil {
		return "", err
	}
	defer root.Close()
	info, err := root.Lstat(id)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("image attachment is not a regular file")
	}
	return filepath.Join(s.path, id), nil
}

// FileHandleText is the same fallback projection sent to every model route.
// No generic file upload service is implied: an unavailable readable path is
// stated explicitly rather than pretending that the model has read the bytes.
func FileHandleText(block core.Block) (string, error) {
	var ref struct {
		AttachmentID string `json:"attachmentId"`
		ID, Name     string
		Bytes        int64
	}
	if err := json.Unmarshal(core.JSON(block.Source), &ref); err != nil {
		return "", err
	}
	if ref.AttachmentID == "" {
		ref.AttachmentID = ref.ID
	}
	id := strings.TrimPrefix(ref.AttachmentID, "sha256:")
	if !validID.MatchString(id) || ref.Bytes < 0 {
		return "", errors.New("invalid file attachment reference")
	}
	if ref.Name == "" {
		ref.Name = block.Name
	}
	identity := fmt.Sprintf("File %s (%d bytes, sha256:%s)", quotedAttachment(ref.Name), ref.Bytes, id[:8])
	return "[" + identity + " was uploaded, but the current execution environment cannot access a readable path. Report that limitation if its contents are needed; do not claim to have read it.]", nil
}

func quotedAttachment(value string) string {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	raw := strings.TrimSuffix(out.String(), "\n")
	var quoted strings.Builder
	for i := 0; i < len(raw); {
		if raw[i] == '\\' && i+1 < len(raw) {
			if strings.HasPrefix(raw[i:], `\u2028`) {
				quoted.WriteRune('\u2028')
				i += 6
				continue
			}
			if strings.HasPrefix(raw[i:], `\u2029`) {
				quoted.WriteRune('\u2029')
				i += 6
				continue
			}
			// Consume a complete escape, including a literal backslash, so
			// a filename containing \\u2028 is never interpreted as Unicode.
			quoted.WriteString(raw[i : i+2])
			i += 2
			continue
		}
		quoted.WriteByte(raw[i])
		i++
	}
	return quoted.String()
}

func RequestImageHandle(ref ImageRef, width, height int, access string) string {
	identity := ref.AttachmentID
	if ref.Name != "" {
		identity = quotedAttachment(ref.Name) + " (" + identity + ")"
	}
	preview := fmt.Sprintf("Image %s; request preview %dx%dpx.", identity, width, height)
	if access == "" {
		return preview + " It may be resized or re-encoded; source dimensions, format, and byte size may differ."
	}
	ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif"}[ref.MediaType]
	return preview + fmt.Sprintf(" Normalized copy (read-only; may be resized or re-encoded): %s (%dx%dpx, %s).", quotedAttachment(access), ref.Width, ref.Height, ref.MediaType) + " Source dimensions, format, and byte size may differ. Copy to a writable path ending in " + ext + " before editing."
}

func TextOnlyImageHandle(ref ImageRef) string {
	id := strings.TrimPrefix(ref.AttachmentID, "sha256:")
	return "[image omitted because this model accepts text only; attachment sha256:" + id[:min(8, len(id))] + "]"
}

type projectedModel struct {
	store   *Store
	model   core.Model
	project ImageProjector
}

func (s *Store) ResolveProjected(model core.Model, project ImageProjector) core.Model {
	return projectedModel{s, model, project}
}

func (r projectedModel) Stream(ctx context.Context, request core.Request, emit func(core.Delta)) (core.Response, error) {
	request.Messages = append([]core.Message(nil), request.Messages...)
	for i, message := range request.Messages {
		blocks, err := r.blocks(ctx, request, message.Content)
		if err != nil {
			return core.Response{}, err
		}
		request.Messages[i].Content = blocks
	}
	return r.model.Stream(ctx, request, emit)
}
func (r projectedModel) blocks(ctx context.Context, req core.Request, blocks []core.Block) ([]core.Block, error) {
	out := make([]core.Block, 0, len(blocks))
	for _, block := range blocks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch block.Type {
		case "image":
			ref, durable, err := r.store.ImageReference(ctx, block)
			if err != nil {
				return nil, err
			}
			plan := ImageProjection{}
			if r.project != nil {
				plan, err = r.project(ctx, req, ref, durable)
				if err != nil {
					return nil, err
				}
			}
			if plan.Text != "" {
				out = append(out, core.Block{Type: "text", Text: plan.Text})
			}
			if plan.Omit {
				continue
			}
			if durable && r.store == nil {
				return nil, errors.New("image attachment service unavailable")
			}
			if durable && plan.Target != nil {
				version, err := r.store.RequestImage(ctx, ref, *plan.Target)
				if err != nil {
					return nil, err
				}
				block.Source = map[string]any{"type": "base64", "media_type": version.MediaType, "data": base64.StdEncoding.EncodeToString(version.Data)}
			} else if durable {
				source, err := r.store.resolveSource(block.Source)
				if err != nil {
					return nil, err
				}
				block.Source = source
			}
		case "file":
			text, err := FileHandleText(block)
			if err != nil {
				return nil, err
			}
			block = core.Block{Type: "text", Text: text}
		case "tool_result":
			content, err := r.content(ctx, req, block.Content)
			if err != nil {
				return nil, err
			}
			block.Content = content
		}
		out = append(out, block)
	}
	return out, nil
}
func (r projectedModel) content(ctx context.Context, req core.Request, value any) (any, error) {
	switch values := value.(type) {
	case []core.Block:
		return r.blocks(ctx, req, values)
	case []json.RawMessage:
		var entries []any
		if err := json.Unmarshal(core.JSON(values), &entries); err != nil {
			return nil, err
		}
		return r.content(ctx, req, entries)
	case json.RawMessage:
		var entries []any
		if json.Unmarshal(values, &entries) == nil {
			return r.content(ctx, req, entries)
		}
		return value, nil
	case []any:
		out := make([]any, 0, len(values))
		for _, entry := range values {
			var block core.Block
			switch object := entry.(type) {
			case core.Block:
				block = object
			case map[string]any, json.RawMessage:
				if err := json.Unmarshal(core.JSON(object), &block); err != nil {
					out = append(out, entry)
					continue
				}
			default:
				out = append(out, entry)
				continue
			}
			if block.Type != "image" && block.Type != "file" && block.Type != "tool_result" {
				out = append(out, entry)
				continue
			}
			blocks, err := r.blocks(ctx, req, []core.Block{block})
			if err != nil {
				return nil, err
			}
			for _, b := range blocks {
				out = append(out, b)
			}
		}
		return out, nil
	default:
		// Native tools may supply other JSON-compatible typed slices. Replay
		// normalizes those to []any; use that same shape without touching a
		// structured object or scalar result.
		var entries []any
		if raw, err := json.Marshal(value); err == nil && json.Unmarshal(raw, &entries) == nil && entries != nil {
			return r.content(ctx, req, entries)
		}
		return value, nil
	}
}
