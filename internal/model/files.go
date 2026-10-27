package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"dsgo/internal/core"
)

const maxUploadBytes = 64 << 20
const defaultExpiry = 7 * 24 * time.Hour

type File struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	SizeBytes int64  `json:"size_bytes"`
	CreatedAt string `json:"created_at"`
	Filename  string `json:"filename"`
	MIMEType  string `json:"mime_type"`
}
type cachedFile struct {
	file    File
	expires time.Time
}

// UploadImage uploads prepared image bytes to the native Messages Files API.
// Uploads have an explicit seven-day expiry, matching upstream's default.
// It does not read paths, resize images, or silently send other file formats.
func (c *Client) UploadImage(ctx context.Context, data []byte, mediaType, filename string) (File, error) {
	if err := c.ready(); err != nil {
		return File{}, err
	}
	if len(data) == 0 || len(data) > maxUploadBytes {
		return File{}, &Error{Code: "IMAGE_LIMIT"}
	}
	if mediaType != "image/png" && mediaType != "image/jpeg" && mediaType != "image/gif" && mediaType != "image/webp" {
		return File{}, invalid()
	}
	filename = filepath.Base(filename)
	if filename == "" || filename == "." || strings.ContainsAny(filename, "\r\n\x00") || len(filename) > 512 {
		return File{}, invalid()
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("expires_after[anchor]", "created_at"); err != nil {
		return File{}, invalid()
	}
	if err := writer.WriteField("expires_after[seconds]", "604800"); err != nil {
		return File{}, invalid()
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(filename)))
	header.Set("Content-Type", mediaType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return File{}, invalid()
	}
	if _, err = part.Write(data); err != nil {
		return File{}, invalid()
	}
	if err = writer.Close(); err != nil {
		return File{}, invalid()
	}
	uploadCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	response, err := c.request(uploadCtx, http.MethodPost, "/files", &body, writer.FormDataContentType(), true)
	if err != nil {
		return File{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return File{}, c.responseError(response)
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return File{}, &Error{Code: "TRANSPORT"}
	}
	var file File
	if json.Unmarshal(encoded, &file) != nil || file.ID == "" || file.Type != "file" || file.SizeBytes < 0 || file.Filename == "" || file.MIMEType == "" {
		return File{}, &Error{Code: "INVALID_RESPONSE"}
	}
	if _, err := time.Parse(time.RFC3339, file.CreatedAt); err != nil {
		return File{}, &Error{Code: "INVALID_RESPONSE"}
	}
	return file, nil
}

// DeleteFile explicitly removes a known upload and its local cache references.
func (c *Client) DeleteFile(ctx context.Context, id string) error {
	if err := c.ready(); err != nil {
		return err
	}
	if id == "" {
		return invalid()
	}
	response, err := c.request(ctx, http.MethodDelete, "/files/"+url.PathEscape(id), nil, "application/json", true)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return c.responseError(response)
	}
	var result struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result) != nil || result.ID != id || result.Type != "file_deleted" {
		return &Error{Code: "INVALID_RESPONSE"}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, file := range c.files {
		if file.file.ID == id {
			delete(c.files, key)
		}
	}
	return nil
}

func (c *Client) prepareImages(ctx context.Context, body *wireRequest) (hasFiles, uploaded bool, err error) {
	original := cloneWireRequest(*body)
	total := 0
	err = imageBlocks(body, func(block *core.Block) error {
		source := sourceOf(block)
		if source["type"] == "file" {
			hasFiles = true
			return nil
		}
		data, err := base64.StdEncoding.DecodeString(source["data"])
		if err != nil {
			return invalid()
		}
		total += len(data)
		if total > 128<<20 {
			return &Error{Code: "IMAGE_LIMIT"}
		}
		if c.disableFiles {
			return nil
		}
		hash := sha256.Sum256(append([]byte(source["media_type"]+"\x00"), data...))
		key := hex.EncodeToString(hash[:])
		c.mu.Lock()
		cached, exists := c.files[key]
		c.mu.Unlock()
		if !exists || time.Until(cached.expires) < time.Hour {
			ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif"}[source["media_type"]]
			file, err := c.UploadImage(ctx, data, source["media_type"], "dsgo-"+key[:16]+ext)
			if err != nil {
				return err
			}
			cached = cachedFile{file: file, expires: time.Now().Add(defaultExpiry)}
			c.mu.Lock()
			if len(c.files) >= 64 {
				for old := range c.files {
					delete(c.files, old)
					break
				}
			}
			c.files[key] = cached
			c.mu.Unlock()
		}
		block.Source = map[string]string{"type": "file", "file_id": cached.file.ID}
		hasFiles = true
		uploaded = true
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return false, false, ctx.Err()
		}
		// Upstream reconstructs the complete request with inline versions when
		// Files resolution fails; existing explicit file ids remain references.
		*body = original
		if imageBytes(original) > maxInlineImageBytes {
			return false, false, &Error{Code: "IMAGE_LIMIT"}
		}
		return containsFile(original), false, nil
	}
	return hasFiles, uploaded, nil
}
