// Package attachments stores image bytes separately from append-only events.
package attachments

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"dsgo/internal/core"
)

const maxBytes = 64 << 20

var validID = regexp.MustCompile(`^[0-9a-f]{64}$`)

type Store struct {
	path        string
	compression chan struct{}
}

func Open(root string) (*Store, error) {
	p := filepath.Join(root, "attachments")
	if err := os.MkdirAll(p, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("attachment store must be a real directory")
	}
	// Establish the attachment-directory entry before a future object ref can
	// be recorded in an already durable session log.
	parent, err := os.Open(filepath.Dir(p))
	if err != nil {
		return nil, err
	}
	err = parent.Sync()
	_ = parent.Close()
	if err != nil {
		return nil, err
	}
	return &Store{path: p, compression: make(chan struct{}, 2)}, nil
}
func (s *Store) Put(data []byte, mediaType string) (map[string]any, error) {
	if len(data) == 0 || len(data) > maxBytes {
		return nil, errors.New("image must contain 1..64 MiB")
	}
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return nil, errors.New("unsupported image media type")
	}
	digest := sha256.Sum256(data)
	id := hex.EncodeToString(digest[:])
	root, err := os.OpenRoot(s.path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	// Publish only a fully written, fsynced object. Writing the digest pathname
	// directly exposes a partially written file to a concurrent equal upload.
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	temporary := ".staging-" + hex.EncodeToString(nonce[:])
	f, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	if err = root.Link(temporary, id); errors.Is(err, os.ErrExist) {
		if _, err = s.Read(id); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if err = root.Remove(temporary); err != nil {
		return nil, err
	}
	if err = root.Chmod(id, 0400); err != nil {
		return nil, err
	}
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	err = directory.Sync()
	_ = directory.Close()
	if err != nil {
		return nil, err
	}
	return map[string]any{"type": "dsgo_attachment", "id": id, "media_type": mediaType}, nil
}
func (s *Store) Read(id string) ([]byte, error) {
	if !validID.MatchString(id) {
		return nil, errors.New("invalid attachment id")
	}
	root, err := os.OpenRoot(s.path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Lstat(id)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxBytes {
		return nil, errors.New("invalid attachment object")
	}
	f, err := root.Open(id)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != id {
		return nil, errors.New("attachment checksum mismatch")
	}
	return b, nil
}

// Resolve wraps a transport at the credential-independent model boundary.
// Session logs keep stable references; only the outbound request gets base64.
func (s *Store) Resolve(model core.Model) core.Model { return resolved{store: s, model: model} }

type resolved struct {
	store *Store
	model core.Model
}

func (r resolved) Stream(ctx context.Context, request core.Request, emit func(core.Delta)) (core.Response, error) {
	request.Messages = append([]core.Message(nil), request.Messages...)
	for i, message := range request.Messages {
		if err := ctx.Err(); err != nil {
			return core.Response{}, err
		}
		blocks, err := r.store.resolveBlocks(ctx, message.Content)
		if err != nil {
			return core.Response{}, err
		}
		request.Messages[i].Content = blocks
	}
	return r.model.Stream(ctx, request, emit)
}

func (s *Store) resolveBlocks(ctx context.Context, blocks []core.Block) ([]core.Block, error) {
	out := append([]core.Block(nil), blocks...)
	for i := range out {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch out[i].Type {
		case "image":
			source, err := s.resolveSource(out[i].Source)
			if err != nil {
				return nil, err
			}
			out[i].Source = source
		case "tool_result":
			content, err := s.resolveToolContent(ctx, out[i].Content)
			if err != nil {
				return nil, err
			}
			out[i].Content = content
		}
	}
	return out, nil
}

func (s *Store) resolveSource(value any) (any, error) {
	var source map[string]any
	switch v := value.(type) {
	case map[string]any:
		source = v
	case map[string]string:
		source = make(map[string]any, len(v))
		for k, entry := range v {
			source[k] = entry
		}
	default:
		return value, nil
	}
	if source["type"] != "dsgo_attachment" {
		return value, nil
	}
	id, ok := source["id"].(string)
	if !ok {
		return nil, errors.New("invalid attachment source")
	}
	mediaType, ok := source["media_type"].(string)
	if !ok || !supportedImageType(mediaType) {
		return nil, errors.New("invalid attachment media type")
	}
	data, err := s.Read(id)
	if err != nil {
		return nil, fmt.Errorf("load attachment: %w", err)
	}
	return map[string]any{"type": "base64", "media_type": mediaType, "data": base64.StdEncoding.EncodeToString(data)}, nil
}

// Tool content is restored as []any/maps by JSONL replay and as []core.Block
// during a live turn. Handle both without mutating either caller-owned shape.
func (s *Store) resolveToolContent(ctx context.Context, value any) (any, error) {
	switch content := value.(type) {
	case []core.Block:
		return s.resolveBlocks(ctx, content)
	case []any:
		out := make([]any, len(content))
		for i, entry := range content {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			block, ok := entry.(map[string]any)
			if !ok {
				out[i] = entry
				continue
			}
			copy := make(map[string]any, len(block))
			for k, v := range block {
				copy[k] = v
			}
			if block["type"] == "image" {
				source, err := s.resolveSource(block["source"])
				if err != nil {
					return nil, err
				}
				copy["source"] = source
			} else if block["type"] == "tool_result" {
				nested, err := s.resolveToolContent(ctx, block["content"])
				if err != nil {
					return nil, err
				}
				copy["content"] = nested
			}
			out[i] = copy
		}
		return out, nil
	default:
		return value, nil
	}
}

func supportedImageType(mediaType string) bool {
	return mediaType == "image/png" || mediaType == "image/jpeg" || mediaType == "image/gif" || mediaType == "image/webp"
}
