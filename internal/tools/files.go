package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"dsgo/internal/core"
)

const maxFileBytes = 16 << 20
const maxOutputBytes = 1 << 20

func (r *Registry) builtins() {
	for _, d := range []struct {
		s core.ToolSpec
		h Handler
	}{
		{spec("list_directory", "List entries without following symlinks. Optional recursive traversal has bounded output.", `{"type":"object","properties":{"path":{"type":"string"},"recursive":{"type":"boolean"},"limit":{"type":"integer"}}}`), listDirectory},
		{spec("search_files", "Search UTF-8 files with a Go regular expression. Symlinks are not traversed; output is bounded.", `{"type":"object","required":["pattern"],"properties":{"path":{"type":"string"},"pattern":{"type":"string"},"glob":{"type":"string"},"limit":{"type":"integer"}}}`), searchFiles},
	} {
		if err := r.Register(d.s, d.h); err != nil {
			panic(err)
		}
	}
}

func absolute(tc core.ToolContext, p string) (string, error) {
	if p == "" {
		p = "."
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(tc.CWD, p)
	}
	return filepath.Abs(p)
}

func regularFile(path string) (*os.File, error) {
	f, st, err := openRegularFile(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("path is not a regular file")
	}
	if st.Size() > maxFileBytes {
		f.Close()
		return nil, fmt.Errorf("file exceeds %d byte limit", maxFileBytes)
	}
	return f, nil
}

func canonicalParent(path string) (string, error) {
	parent := filepath.Dir(path)
	var tails []string
	for {
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			for i := len(tails) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tails[i])
			}
			return filepath.Join(resolved, filepath.Base(path)), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		next := filepath.Dir(parent)
		if next == parent {
			return "", err
		}
		tails = append(tails, filepath.Base(parent))
		parent = next
	}
}

func inside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel)
}

// writeRoot uses os.Root so a symlink swapped after policy evaluation cannot redirect a write.
func writeRoot(ctx context.Context, tc core.ToolContext, name string, input json.RawMessage, path string) (*os.Root, string, error) {
	workspace, err := filepath.EvalSymlinks(workspaceRoot(tc))
	if err != nil {
		return nil, "", err
	}
	resolved, err := canonicalParent(path)
	if err != nil {
		return nil, "", err
	}
	// Reject target symlinks. Otherwise atomic replace would quietly replace the link rather than edit its target.
	if st, e := os.Lstat(resolved); e == nil && st.Mode()&os.ModeSymlink != 0 {
		return nil, "", errors.New("refusing to overwrite a symbolic link")
	}
	mode := tc.Permission
	if mode == "" {
		mode = "workspace-write"
	}
	if mode != "workspace-write" && mode != "read-only" && mode != "full-access" {
		return nil, "", errors.New("unknown permission mode")
	}
	if mode != "full-access" && (mode == "read-only" || !inside(workspace, resolved)) {
		if err := ask(ctx, tc, name, "Allow one write to "+resolved+" outside the standing file policy?", input); err != nil {
			return nil, "", err
		}
		return openAncestor(resolved)
	}
	if mode == "full-access" {
		return openAncestor(resolved)
	}
	rel, err := filepath.Rel(workspace, resolved)
	if err != nil {
		return nil, "", err
	}
	root, err := os.OpenRoot(workspace)
	return root, rel, err
}

func openAncestor(path string) (*os.Root, string, error) {
	parent := filepath.Dir(path)
	for {
		root, err := os.OpenRoot(parent)
		if err == nil {
			rel, e := filepath.Rel(parent, path)
			if e != nil {
				root.Close()
				return nil, "", e
			}
			return root, rel, nil
		}
		if !os.IsNotExist(err) {
			return nil, "", err
		}
		next := filepath.Dir(parent)
		if next == parent {
			return nil, "", err
		}
		parent = next
	}
}

func listDirectory(ctx context.Context, tc core.ToolContext, input json.RawMessage) (core.ToolResult, error) {
	var a struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
		Limit     int    `json:"limit"`
	}
	if err := decode(input, &a); err != nil {
		return core.ToolResult{}, err
	}
	if a.Limit == 0 {
		a.Limit = 500
	}
	if a.Limit < 1 || a.Limit > 10000 {
		return core.ToolResult{}, errors.New("limit must be 1..10000")
	}
	p, err := absolute(tc, a.Path)
	if err != nil {
		return core.ToolResult{}, err
	}
	type entry struct {
		Path string `json:"path"`
		Type string `json:"type"`
		Size int64  `json:"size,omitempty"`
	}
	entries := []entry{}
	truncated := false
	err = filepath.WalkDir(p, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == p {
			return nil
		}
		if len(entries) >= a.Limit {
			truncated = true
			return fs.SkipAll
		}
		rel, _ := filepath.Rel(p, path)
		kind := "file"
		if d.IsDir() {
			kind = "directory"
		}
		if d.Type()&os.ModeSymlink != 0 {
			kind = "symlink"
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		entries = append(entries, entry{Path: rel, Type: kind, Size: info.Size()})
		if d.IsDir() && !a.Recursive {
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		return core.ToolResult{}, err
	}
	return result(map[string]any{"path": p, "entries": entries, "truncated": truncated}), nil
}

func searchFiles(ctx context.Context, tc core.ToolContext, input json.RawMessage) (core.ToolResult, error) {
	var a struct {
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
		Glob    string `json:"glob"`
		Limit   int    `json:"limit"`
	}
	if err := decode(input, &a); err != nil {
		return core.ToolResult{}, err
	}
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return core.ToolResult{}, err
	}
	if a.Limit == 0 {
		a.Limit = 200
	}
	if a.Limit < 1 || a.Limit > 10000 {
		return core.ToolResult{}, errors.New("limit must be 1..10000")
	}
	p, err := absolute(tc, a.Path)
	if err != nil {
		return core.ToolResult{}, err
	}
	type match struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Text string `json:"text"`
	}
	matches := []match{}
	truncated := false
	scanned := 0
	skipped := 0
	outBytes := 0
	err = filepath.WalkDir(p, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			skipped++
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			if path != p && (d.Name() == ".git" || d.Name() == "node_modules") {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if a.Glob != "" {
			ok, e := filepath.Match(a.Glob, d.Name())
			if e != nil {
				return e
			}
			if !ok {
				return nil
			}
		}
		f, e := regularFile(path)
		if e != nil {
			skipped++
			return nil
		}
		defer f.Close()
		scanned++
		s := bufio.NewScanner(f)
		s.Buffer(make([]byte, 4096), maxOutputBytes)
		line := 0
		for s.Scan() {
			if err := ctx.Err(); err != nil {
				return err
			}
			line++
			b := s.Bytes()
			if bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b) {
				skipped++
				break
			}
			if re.Match(b) {
				if len(matches) >= a.Limit || outBytes+len(b) > maxOutputBytes {
					truncated = true
					return fs.SkipAll
				}
				rel, _ := filepath.Rel(tc.CWD, path)
				matches = append(matches, match{Path: rel, Line: line, Text: string(b)})
				outBytes += len(b)
			}
		}
		if s.Err() != nil {
			skipped++
		}
		return nil
	})
	if err != nil {
		return core.ToolResult{}, err
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].Path < matches[j].Path })
	return result(map[string]any{"matches": matches, "truncated": truncated, "files_scanned": scanned, "files_skipped": skipped}), nil
}
