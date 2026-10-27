package artifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const MaxFileBytes = 2 << 20
const maxSnapshotBytes = 256 << 20
const maxScanFiles = 100000

type FileState struct {
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Mode      uint32 `json:"mode"`
	Binary    bool   `json:"binary,omitempty"`
	Oversized bool   `json:"oversized,omitempty"`
}
type Snapshot struct {
	CWD   string               `json:"cwd"`
	Files map[string]FileState `json:"files"`
}

// Capture never follows workspace symlinks and never traverses .git. Bytes are
// read through os.Root, so a concurrent path replacement cannot escape CWD.
func (s *Store) Capture(ctx context.Context, cwd string) (Snapshot, error) {
	cwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return Snapshot{}, err
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return Snapshot{}, err
	}
	r, err := os.OpenRoot(cwd)
	if err != nil {
		return Snapshot{}, err
	}
	defer r.Close()
	result := Snapshot{CWD: cwd, Files: map[string]FileState{}}
	var total int64
	err = fs.WalkDir(r.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if walkErr != nil {
			return walkErr
		}
		if path == "." {
			return nil
		}
		absolute := filepath.Join(cwd, path)
		if entry.Name() == ".git" || absolute == s.path {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if len(result.Files) >= maxScanFiles {
			return errors.New("workspace snapshot exceeds file-count limit")
		}
		info, err := r.Lstat(path)
		if err != nil {
			return err
		}
		var data []byte
		var state FileState
		state.Mode = uint32(info.Mode())
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := r.Readlink(path)
			if err != nil {
				return err
			}
			data = []byte(link)
			state.Size = int64(len(data))
		} else {
			if !info.Mode().IsRegular() {
				return nil
			}
			if info.Size() > maxSnapshotBytes-total {
				return errors.New("workspace snapshot exceeds byte limit")
			}
			f, err := r.Open(path)
			if err != nil {
				return err
			}
			h := sha256.New()
			var retained bytes.Buffer
			buf := make([]byte, 64<<10)
			var nread int64
			for {
				if err = ctx.Err(); err != nil {
					break
				}
				n, e := f.Read(buf)
				if n > 0 {
					nread += int64(n)
					total += int64(n)
					if total > maxSnapshotBytes {
						err = errors.New("workspace snapshot exceeds byte limit")
						break
					}
					h.Write(buf[:n])
					if nread <= MaxFileBytes {
						retained.Write(buf[:n])
					}
				}
				if e != nil {
					if !errors.Is(e, io.EOF) {
						err = e
					}
					break
				}
			}
			closeErr := f.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
			state.Digest = hex.EncodeToString(h.Sum(nil))
			state.Size = nread
			state.Oversized = nread > MaxFileBytes
			if !state.Oversized {
				data = retained.Bytes()
			}
		}
		if !state.Oversized {
			state.Binary = bytes.IndexByte(data, 0) >= 0
			state.Digest, err = s.putBlob(data)
			if err != nil {
				return err
			}
		}
		result.Files[filepath.ToSlash(path)] = state
		return nil
	})
	return result, err
}

type ChangedFile struct {
	Path      string `json:"path"`
	Display   string `json:"display"`
	Added     int    `json:"added"`
	Deleted   int    `json:"deleted"`
	Binary    bool   `json:"binary,omitempty"`
	Oversized bool   `json:"oversized,omitempty"`
}
type Summary struct {
	Turn    int           `json:"turn"`
	CWD     string        `json:"cwd"`
	Files   []ChangedFile `json:"files"`
	Total   int           `json:"total"`
	Added   int           `json:"added"`
	Deleted int           `json:"deleted"`
}
type Changes struct {
	Summary Summary  `json:"summary"`
	Before  Snapshot `json:"before"`
	After   Snapshot `json:"after"`
}
type Hunk struct {
	OldStart int      `json:"oldStart"`
	OldLines int      `json:"oldLines"`
	NewStart int      `json:"newStart"`
	NewLines int      `json:"newLines"`
	Lines    []string `json:"lines"`
}
type Diff struct {
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	Display string `json:"display"`
	Before  bool   `json:"before,omitempty"`
	After   bool   `json:"after,omitempty"`
	Hunks   []Hunk `json:"hunks,omitempty"`
	Coarse  bool   `json:"coarse,omitempty"`
}

func (s *Store) Compare(ctx context.Context, before, after Snapshot, turn int) (Changes, error) {
	if before.CWD != after.CWD {
		return Changes{}, errors.New("workspace changed during turn")
	}
	out := Changes{Summary: Summary{Turn: turn, CWD: before.CWD, Files: []ChangedFile{}}, Before: before, After: after}
	paths := map[string]bool{}
	for p := range before.Files {
		paths[p] = true
	}
	for p := range after.Files {
		paths[p] = true
	}
	ordered := make([]string, 0, len(paths))
	for p := range paths {
		ordered = append(ordered, p)
	}
	sort.Strings(ordered)
	for _, p := range ordered {
		if err := ctx.Err(); err != nil {
			return Changes{}, err
		}
		b, bok := before.Files[p]
		a, aok := after.Files[p]
		if bok && aok && a.Digest == b.Digest && a.Mode == b.Mode {
			continue
		}
		file := ChangedFile{Path: p, Display: p, Binary: b.Binary || a.Binary, Oversized: b.Oversized || a.Oversized}
		if !file.Binary && !file.Oversized {
			bd, ad, err := s.contents(before, after, p)
			if err != nil {
				return Changes{}, err
			}
			ops, _ := compareLines(ctx, splitLines(bd), splitLines(ad))
			for _, op := range ops {
				if op.kind == '+' {
					file.Added++
				}
				if op.kind == '-' {
					file.Deleted++
				}
			}
		}
		out.Summary.Total++
		out.Summary.Added += file.Added
		out.Summary.Deleted += file.Deleted
		if len(out.Summary.Files) < 500 {
			out.Summary.Files = append(out.Summary.Files, file)
		}
	}
	return out, nil
}
func (s *Store) contents(before, after Snapshot, path string) ([]byte, []byte, error) {
	var bd, ad []byte
	var err error
	if b, ok := before.Files[path]; ok {
		bd, err = s.blob(b.Digest)
		if err != nil {
			return nil, nil, err
		}
	}
	if a, ok := after.Files[path]; ok {
		ad, err = s.blob(a.Digest)
		if err != nil {
			return nil, nil, err
		}
	}
	return bd, ad, nil
}
func (s *Store) SaveChanges(session, turnID string, changes Changes) error {
	if session == "" || turnID == "" {
		return errors.New("workspace changes require session and turn")
	}
	b, err := json.Marshal(changes)
	if err != nil {
		return err
	}
	return s.write(filepath.Join("changes", ownerKey(session+"\x00"+turnID)+".json"), b, true)
}
func (s *Store) Changes(session, turnID string) (Changes, error) {
	b, err := s.read(filepath.Join("changes", ownerKey(session+"\x00"+turnID)+".json"), 64<<20)
	if err != nil {
		return Changes{}, err
	}
	var result Changes
	err = json.Unmarshal(b, &result)
	return result, err
}
func (s *Store) Diff(ctx context.Context, changes Changes, index int) (Diff, error) {
	if index < 0 || index >= len(changes.Summary.Files) {
		return Diff{}, errors.New("workspace file index out of range")
	}
	f := changes.Summary.Files[index]
	d := Diff{Path: f.Path, Display: f.Display}
	if f.Oversized {
		d.Kind = "oversized"
		return d, nil
	}
	if f.Binary {
		d.Kind = "binary"
		return d, nil
	}
	_, d.Before = changes.Before.Files[f.Path]
	_, d.After = changes.After.Files[f.Path]
	bd, ad, err := s.contents(changes.Before, changes.After, f.Path)
	if err != nil {
		return Diff{}, err
	}
	ops, coarse := compareLines(ctx, splitLines(bd), splitLines(ad))
	if err = ctx.Err(); err != nil {
		return Diff{}, err
	}
	d.Kind = "text"
	d.Coarse = coarse
	d.Hunks = hunks(ops)
	return d, nil
}

type lineOp struct {
	kind byte
	text string
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := strings.Split(string(b), "\n")
	if s[len(s)-1] == "" {
		s = s[:len(s)-1]
	}
	return s
}

// A bounded LCS yields exact counts for common edits. Large or expensive cases
// degrade to whole-file replacement, the official comparison's coarse fallback.
func compareLines(ctx context.Context, a, b []string) ([]lineOp, bool) {
	coarse := func() ([]lineOp, bool) {
		ops := make([]lineOp, 0, len(a)+len(b))
		for _, s := range a {
			ops = append(ops, lineOp{'-', s})
		}
		for _, s := range b {
			ops = append(ops, lineOp{'+', s})
		}
		return ops, true
	}
	// Peel equal edges before allocating a comparison matrix.
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	aa, bb := a[prefix:len(a)-suffix], b[prefix:len(b)-suffix]
	if int64(len(aa)+1)*int64(len(bb)+1) > 4_000_000 {
		return coarse()
	}
	w := len(bb) + 1
	matrix := make([]uint32, (len(aa)+1)*w)
	deadline := time.Now().Add(100 * time.Millisecond)
	for i := len(aa) - 1; i >= 0; i-- {
		if i%64 == 0 && (ctx.Err() != nil || time.Now().After(deadline)) {
			return coarse()
		}
		for j := len(bb) - 1; j >= 0; j-- {
			if aa[i] == bb[j] {
				matrix[i*w+j] = matrix[(i+1)*w+j+1] + 1
			} else {
				matrix[i*w+j] = max(matrix[(i+1)*w+j], matrix[i*w+j+1])
			}
		}
	}
	ops := make([]lineOp, 0, len(a)+len(b))
	for _, v := range a[:prefix] {
		ops = append(ops, lineOp{' ', v})
	}
	i, j := 0, 0
	for i < len(aa) || j < len(bb) {
		if i < len(aa) && j < len(bb) && aa[i] == bb[j] {
			ops = append(ops, lineOp{' ', aa[i]})
			i++
			j++
		} else if i < len(aa) && (j == len(bb) || matrix[(i+1)*w+j] >= matrix[i*w+j+1]) {
			ops = append(ops, lineOp{'-', aa[i]})
			i++
		} else {
			ops = append(ops, lineOp{'+', bb[j]})
			j++
		}
	}
	for _, v := range a[len(a)-suffix:] {
		ops = append(ops, lineOp{' ', v})
	}
	return ops, false
}

func hunks(ops []lineOp) []Hunk {
	result := []Hunk{}
	old, new := make([]int, len(ops)+1), make([]int, len(ops)+1)
	old[0], new[0] = 1, 1
	for i, op := range ops {
		old[i+1], new[i+1] = old[i], new[i]
		if op.kind != '+' {
			old[i+1]++
		}
		if op.kind != '-' {
			new[i+1]++
		}
	}
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := max(0, i-3)
		end := i + 1
		for j := i + 1; j < len(ops); j++ {
			if ops[j].kind != ' ' {
				if j-end > 6 {
					break
				}
				end = j + 1
			}
		}
		end = min(len(ops), end+3)
		h := Hunk{OldStart: old[start], NewStart: new[start], Lines: []string{}}
		for _, op := range ops[start:end] {
			h.Lines = append(h.Lines, string(op.kind)+op.text)
			if op.kind != '+' {
				h.OldLines++
			}
			if op.kind != '-' {
				h.NewLines++
			}
		}
		result = append(result, h)
		i = end
	}
	return result
}
