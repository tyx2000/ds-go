// Package artifacts holds recoverable tool output and workspace comparisons.
// Deliverables themselves remain at their source paths, as in the official app.
package artifacts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var spillPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

type Store struct{ path string }

func Open(root string) (*Store, error) {
	p := filepath.Join(root, "artifacts")
	for _, dir := range []string{p, filepath.Join(p, "blobs"), filepath.Join(p, "spills"), filepath.Join(p, "changes")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("artifact storage requires real directories")
		}
	}
	return &Store{path: p}, nil
}

func (s *Store) write(name string, data []byte, replace bool) error {
	r, err := os.OpenRoot(s.path)
	if err != nil {
		return err
	}
	defer r.Close()
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	tmp := filepath.Dir(name) + "/.tmp-" + hex.EncodeToString(nonce[:])
	f, err := r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer r.Remove(tmp)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if x := f.Close(); err == nil {
		err = x
	}
	if err != nil {
		return err
	}
	if !replace {
		// A content-addressed target is immutable. Reuse only verified bytes.
		if existing, e := s.read(name, int64(len(data))+1); e == nil {
			if string(existing) != string(data) {
				return errors.New("artifact digest collision or corrupted object")
			}
			return nil
		} else if !errors.Is(e, os.ErrNotExist) {
			return e
		}
	}
	if err = r.Rename(tmp, name); err != nil {
		return err
	}
	d, err := r.Open(filepath.Dir(name))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *Store) read(name string, limit int64) ([]byte, error) {
	r, err := os.OpenRoot(s.path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	info, err := r.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("invalid artifact object")
	}
	f, err := r.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("artifact exceeds read limit")
	}
	return b, nil
}

func (s *Store) putBlob(data []byte) (string, error) {
	d := sha256.Sum256(data)
	id := hex.EncodeToString(d[:])
	return id, s.write(filepath.Join("blobs", id), data, false)
}
func (s *Store) blob(id string) ([]byte, error) {
	if !digestPattern.MatchString(id) {
		return nil, errors.New("invalid artifact digest")
	}
	b, err := s.read(filepath.Join("blobs", id), 2<<20)
	if err != nil {
		return nil, err
	}
	d := sha256.Sum256(b)
	if hex.EncodeToString(d[:]) != id {
		return nil, errors.New("artifact integrity check failed")
	}
	return b, nil
}
func ownerKey(owner string) string {
	d := sha256.Sum256([]byte(owner))
	return hex.EncodeToString(d[:])
}

type Spill struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Tool      string `json:"tool,omitempty"`
	Source    any    `json:"source,omitempty"`
	TurnID    string `json:"turn_id,omitempty"`
	Path      string `json:"path"`
	Bytes     int    `json:"bytes"`
	SHA256    string `json:"sha256"`
}

func (s *Store) Spill(session, turn, tool, text string) (Spill, error) {
	return s.spill(session, turn, tool, nil, text)
}

// SpillContext retains a sourced context capture without fabricating a tool call.
func (s *Store) SpillContext(session string, source any, text string) (Spill, error) {
	return s.spill(session, "", "", source, text)
}

func (s *Store) spill(session, turn, tool string, source any, text string) (Spill, error) {
	if session == "" {
		return Spill{}, errors.New("spill requires a session owner")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Spill{}, err
	}
	id := hex.EncodeToString(nonce[:])
	name := ownerKey(session) + "-" + id
	d := sha256.Sum256([]byte(text))
	ref := Spill{ID: id, SessionID: session, TurnID: turn, Tool: tool, Source: source, Path: filepath.Join(s.path, "spills", name+".txt"), Bytes: len(text), SHA256: hex.EncodeToString(d[:])}
	b, err := json.Marshal(ref)
	if err != nil {
		return Spill{}, err
	}
	if err := s.write(filepath.Join("spills", name+".txt"), []byte(text), false); err != nil {
		return Spill{}, err
	}
	if err := s.write(filepath.Join("spills", name+".json"), b, false); err != nil {
		return Spill{}, err
	}
	return ref, nil
}

func (s *Store) ReadSpill(session, id string, offset, limit int) (Spill, string, int, error) {
	if session == "" || !spillPattern.MatchString(id) || offset < 0 || limit < 1 || limit > 1<<20 {
		return Spill{}, "", 0, errors.New("invalid spill read")
	}
	name := ownerKey(session) + "-" + id
	b, err := s.read(filepath.Join("spills", name+".json"), 1<<20)
	if err != nil {
		return Spill{}, "", 0, err
	}
	var ref Spill
	if err = json.Unmarshal(b, &ref); err != nil {
		return Spill{}, "", 0, err
	}
	if ref.SessionID != session || ref.ID != id || ref.Bytes < 0 {
		return Spill{}, "", 0, errors.New("spill owner mismatch")
	}
	b, err = s.read(filepath.Join("spills", name+".txt"), int64(ref.Bytes))
	if err != nil {
		return Spill{}, "", 0, err
	}
	d := sha256.Sum256(b)
	if hex.EncodeToString(d[:]) != ref.SHA256 {
		return Spill{}, "", 0, errors.New("spill integrity check failed")
	}
	if offset > len(b) {
		offset = len(b)
	}
	for offset < len(b) && !utf8.RuneStart(b[offset]) {
		offset++
	}
	end := min(len(b), offset+limit)
	for end > offset && end < len(b) && !utf8.RuneStart(b[end]) {
		end--
	}
	return ref, string(b[offset:end]), end, nil
}

// Retain creates a recoverable display copy. Callers must retain the original
// canonical value for nested program calls. A failed save leaves the text intact.
func (s *Store) Retain(session, turn, tool, text string, maxBytes int) (string, *Spill, error) {
	if maxBytes < 1024 {
		return text, nil, errors.New("spill inline budget must be at least 1024 bytes")
	}
	if len(text) <= maxBytes || tool == "read_file" || tool == "read" {
		return text, nil, nil
	}
	ref, err := s.Spill(session, turn, tool, text)
	if err != nil {
		return text, nil, err
	}
	notice := fmt.Sprintf("\n\n[Output omitted; full %d bytes saved at %s. Read this file with read_file or use /artifacts spill %s.]", len(text), ref.Path, ref.ID)
	budget := maxBytes - len(notice) - len("\n\n[...]\n\n")
	if budget < 0 {
		return text, &ref, errors.New("spill recovery notice exceeds inline budget")
	}
	head := budget * 4 / 5
	tail := budget - head
	for head > 0 && !utf8.RuneStart(text[head]) {
		head--
	}
	start := len(text) - tail
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[:head] + "\n\n[...]\n\n" + text[start:] + notice, &ref, nil
}

type PresentedFile struct {
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

// ResolvePresented performs metadata checks without copying or reading contents.
func ResolvePresented(ctx context.Context, cwd string, files []PresentedFile) ([]PresentedFile, error) {
	if len(files) < 1 || len(files) > 8 {
		return nil, errors.New("present accepts 1 to 8 files")
	}
	out := make([]PresentedFile, 0, len(files))
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.TrimSpace(file.Path) == "" {
			return nil, errors.New("present requires a non-empty file path")
		}
		p := file.Path
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		p, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		st, err := os.Lstat(p)
		if err != nil {
			return nil, fmt.Errorf("cannot present %s: %w", file.Path, err)
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("cannot present %s: not a regular file", file.Path)
		}
		// Canonicalize parents while preserving the final no-symlink check.
		parent, err := filepath.EvalSymlinks(filepath.Dir(p))
		if err != nil {
			return nil, err
		}
		file.Path = filepath.Join(parent, filepath.Base(p))
		st, err = os.Lstat(file.Path)
		if err != nil {
			return nil, err
		}
		if !st.Mode().IsRegular() {
			return nil, errors.New("present source changed during validation")
		}
		out = append(out, file)
	}
	return out, nil
}
