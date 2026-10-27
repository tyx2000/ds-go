package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
		{spec("bash", "Run commands in a persistent bash shell. State, including the current directory and exported environment variables, persists across calls for this agent. Commands have a 300 second deadline. Run long lived commands in the background.", `{"type":"object","required":["command"],"additionalProperties":false,"properties":{"command":{"type":"string"}}}`), r.persistentBash},
		{spec("read_file", "Read a UTF-8 text file. offset is a zero-based line number; limit is number of lines (default 200).", `{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"}}}`), readFile},
		{spec("write_file", "Atomically write a UTF-8 file. Restricted writes outside the workspace require one-time approval.", `{"type":"object","required":["path","content"],"properties":{"path":{"type":"string"},"content":{"type":"string"}}}`), writeFile},
		{spec("edit_file", "Replace an exact text match. Ambiguous matches fail unless replace_all is true.", `{"type":"object","required":["path","old_text","new_text"],"properties":{"path":{"type":"string"},"old_text":{"type":"string"},"new_text":{"type":"string"},"replace_all":{"type":"boolean"}}}`), editFile},
		{spec("list_directory", "List entries without following symlinks. Optional recursive traversal has bounded output.", `{"type":"object","properties":{"path":{"type":"string"},"recursive":{"type":"boolean"},"limit":{"type":"integer"}}}`), listDirectory},
		{spec("search_files", "Search UTF-8 files with a Go regular expression. Symlinks are not traversed; output is bounded.", `{"type":"object","required":["pattern"],"properties":{"path":{"type":"string"},"pattern":{"type":"string"},"glob":{"type":"string"},"limit":{"type":"integer"}}}`), searchFiles},
		{spec("run_shell", "Run a shell command with the selected OS filesystem sandbox. timeout_ms defaults to 120000 (max 600000). Set background for an owner-scoped job; it is canceled when dsgo exits. sandbox full-access requires approval unless already full-access.", `{"type":"object","required":["command"],"properties":{"command":{"type":"string"},"timeout_ms":{"type":"integer"},"background":{"type":"boolean"},"sandbox":{"type":"string","enum":["read-only","workspace-write","full-access"]}}}`), r.runShell},
		{spec("jobs", "Observe or manage this session's background shell jobs. Reading output is non-consuming unless consume is true.", `{"type":"object","required":["action"],"properties":{"action":{"type":"string","enum":["list","output","wait","kill"]},"id":{"type":"string"},"offset":{"type":"integer"},"consume":{"type":"boolean"},"timeout_ms":{"type":"integer"}}}`), r.jobTool},
		{spec("terminal", "Manage this session's real PTY terminals. start runs a shell (optional command); write sends exact input including control characters; resize changes terminal dimensions. read is non-consuming unless consume is true. Terminals stop when dsgo closes.", `{"type":"object","required":["action"],"properties":{"action":{"type":"string","enum":["start","list","read","write","resize","kill"]},"id":{"type":"string"},"command":{"type":"string"},"input":{"type":"string"},"rows":{"type":"integer"},"cols":{"type":"integer"},"offset":{"type":"integer"},"consume":{"type":"boolean"},"wait_ms":{"type":"integer"},"sandbox":{"type":"string","enum":["read-only","workspace-write","full-access"]}}}`), r.terminalTool},
		{spec("run_code", "Execute erasable TypeScript in a fresh Node process. Supports top-level await and return. Await tools.<name>(args) to call the same visible tools and approvals. Direct Node filesystem/process APIs follow the selected OS sandbox. Memory is not retained. timeout_ms defaults to 120000 (max 600000).", `{"type":"object","required":["code"],"properties":{"code":{"type":"string"},"timeout_ms":{"type":"integer"},"sandbox":{"type":"string","enum":["read-only","workspace-write","full-access"]}}}`), r.runCode},
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
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
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

func readText(path string) ([]byte, error) {
	f, err := regularFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxFileBytes {
		return nil, errors.New("file grew beyond size limit")
	}
	if bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b) {
		return nil, errors.New("binary or non-UTF-8 file; use an attachment/preview instead")
	}
	return b, nil
}

func readFile(ctx context.Context, tc core.ToolContext, input json.RawMessage) (core.ToolResult, error) {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := decode(input, &a); err != nil {
		return core.ToolResult{}, err
	}
	if a.Offset < 0 || a.Limit < 0 {
		return core.ToolResult{}, errors.New("offset and limit must be nonnegative")
	}
	if a.Limit == 0 {
		a.Limit = 200
	}
	if a.Limit > 10000 {
		a.Limit = 10000
	}
	p, err := absolute(tc, a.Path)
	if err != nil {
		return core.ToolResult{}, err
	}
	b, err := readText(p)
	if err != nil {
		return core.ToolResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return core.ToolResult{}, err
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	end := a.Offset + a.Limit
	if end > len(lines) {
		end = len(lines)
	}
	if a.Offset > len(lines) {
		a.Offset = len(lines)
	}
	var out strings.Builder
	truncated := false
	for i := a.Offset; i < end; i++ {
		line := fmt.Sprintf("%6d\t%s\n", i+1, lines[i])
		if out.Len()+len(line) > maxOutputBytes {
			truncated = true
			end = i
			break
		}
		out.WriteString(line)
	}
	return result(map[string]any{"path": p, "offset": a.Offset, "next_offset": end, "total_lines": len(lines), "truncated": truncated || end < len(lines), "content": out.String()}), nil
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

func atomicWrite(root *os.Root, path string, b []byte, mode fs.FileMode) error {
	if len(b) > maxFileBytes {
		return errors.New("content exceeds file size limit")
	}
	parent := filepath.Dir(path)
	if err := root.MkdirAll(parent, 0755); err != nil {
		return err
	}
	tmp := filepath.Join(parent, ".dsgo-"+newID()+".tmp")
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return root.Rename(tmp, path)
}

func writeFile(ctx context.Context, tc core.ToolContext, input json.RawMessage) (core.ToolResult, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decode(input, &a); err != nil {
		return core.ToolResult{}, err
	}
	if a.Path == "" {
		return core.ToolResult{}, errors.New("path is empty")
	}
	p, err := absolute(tc, a.Path)
	if err != nil {
		return core.ToolResult{}, err
	}
	root, rel, err := writeRoot(ctx, tc, "write_file", input, p)
	if err != nil {
		return core.ToolResult{}, err
	}
	defer root.Close()
	if err := ctx.Err(); err != nil {
		return core.ToolResult{}, err
	}
	mode := fs.FileMode(0644)
	if st, e := root.Stat(rel); e == nil {
		if !st.Mode().IsRegular() {
			return core.ToolResult{}, errors.New("target is not a regular file")
		}
		mode = st.Mode().Perm()
	}
	if err := atomicWrite(root, rel, []byte(a.Content), mode); err != nil {
		return core.ToolResult{}, err
	}
	return result(map[string]any{"path": p, "bytes": len(a.Content)}), nil
}

func editFile(ctx context.Context, tc core.ToolContext, input json.RawMessage) (core.ToolResult, error) {
	var a struct {
		Path string `json:"path"`
		Old  string `json:"old_text"`
		New  string `json:"new_text"`
		All  bool   `json:"replace_all"`
	}
	if err := decode(input, &a); err != nil {
		return core.ToolResult{}, err
	}
	if a.Path == "" || a.Old == "" {
		return core.ToolResult{}, errors.New("path and old_text must not be empty")
	}
	p, err := absolute(tc, a.Path)
	if err != nil {
		return core.ToolResult{}, err
	}
	root, rel, err := writeRoot(ctx, tc, "edit_file", input, p)
	if err != nil {
		return core.ToolResult{}, err
	}
	defer root.Close()
	f, err := root.Open(rel)
	if err != nil {
		return core.ToolResult{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return core.ToolResult{}, err
	}
	if !st.Mode().IsRegular() || st.Size() > maxFileBytes {
		return core.ToolResult{}, errors.New("target is not a bounded regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return core.ToolResult{}, err
	}
	if !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return core.ToolResult{}, errors.New("target is not UTF-8 text")
	}
	n := strings.Count(string(b), a.Old)
	if n == 0 {
		return core.ToolResult{}, errors.New("old_text was not found; file unchanged")
	}
	if n > 1 && !a.All {
		return core.ToolResult{}, fmt.Errorf("old_text matched %d times; provide unique context or replace_all", n)
	}
	if err := ctx.Err(); err != nil {
		return core.ToolResult{}, err
	}
	updated := strings.ReplaceAll(string(b), a.Old, a.New)
	if err := atomicWrite(root, rel, []byte(updated), st.Mode().Perm()); err != nil {
		return core.ToolResult{}, err
	}
	return result(map[string]any{"path": p, "replacements": n, "bytes": len(updated)}), nil
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
