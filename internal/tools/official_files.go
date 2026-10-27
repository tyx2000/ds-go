package tools

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf16"
	"unicode/utf8"

	"dsgo/internal/core"
)

// Pinned official fs tools share per-owner observations and path serialization.
// A shell or another session cannot make an earlier read a fresh edit basis.
type fileObservation struct {
	Present bool
	Version string
}

var officialFileLocks sync.Map

type fileLine struct {
	Number int    `json:"number"`
	Text   string `json:"text"`
}
type readValue struct {
	Path       string     `json:"path"`
	Offset     int        `json:"offset"`
	Lines      []fileLine `json:"lines"`
	TotalLines int        `json:"totalLines"`
}

func (r *Registry) officialFiles() {
	for _, d := range []struct {
		s core.ToolSpec
		h Handler
	}{
		{spec("read", "Read a UTF-8 text file and return line-numbered content.", `{"type":"object","additionalProperties":false,"required":["file_path"],"properties":{"file_path":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"}}}`), r.officialRead},
		{spec("write", "Create or fully replace a UTF-8 text file. Read an existing file first; unobserved existing files cannot be overwritten.", `{"type":"object","additionalProperties":false,"required":["file_path","content"],"properties":{"file_path":{"type":"string"},"content":{"type":"string"},"sandbox_permissions":{"type":"string","enum":["workspace-write","danger-full-access"]},"justification":{"type":"string"}}}`), r.officialWrite},
		{spec("edit", "Edit an existing UTF-8 text file by replacing literal text. Read the file first unless just created or edited in this session.", `{"type":"object","additionalProperties":false,"required":["file_path","old_string","new_string"],"properties":{"file_path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"},"replace_all":{"type":"boolean"},"sandbox_permissions":{"type":"string","enum":["workspace-write","danger-full-access"]},"justification":{"type":"string"}}}`), r.officialEdit},
	} {
		if err := r.Register(d.s, d.h); err != nil {
			panic(err)
		}
	}
}

func (r *Registry) observed(owner, path string) (fileObservation, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.observations[owner][path]
	return v, ok
}
func (r *Registry) observe(owner, path string, v fileObservation) {
	if owner == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.observations[owner] == nil {
		r.observations[owner] = map[string]fileObservation{}
	}
	r.observations[owner][path] = v
}
func fileLock(path string) func() {
	v, _ := officialFileLocks.LoadOrStore(path, &sync.Mutex{})
	m := v.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}
func fileTarget(tc core.ToolContext, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("file_path must be a non-empty string")
	}
	p, err := absolute(tc, path)
	if err != nil {
		return "", err
	}
	return canonicalParent(p)
}

func fileVersion(ctx context.Context, path string) (fileObservation, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileObservation{}, nil
	}
	if err != nil {
		return fileObservation{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fileObservation{}, err
	}
	if !info.Mode().IsRegular() {
		return fileObservation{}, errors.New("path is not a regular file")
	}
	h := sha256.New()
	b := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return fileObservation{}, err
		}
		n, e := f.Read(b)
		if n > 0 {
			h.Write(b[:n])
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return fileObservation{}, e
		}
	}
	after, err := f.Stat()
	if err != nil {
		return fileObservation{}, err
	}
	if after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return fileObservation{}, errors.New("FS_STALE: file changed while reading; read it again")
	}
	return fileObservation{Present: true, Version: hex.EncodeToString(h.Sum(nil))}, nil
}

func (r *Registry) officialRead(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	var a struct {
		Path          string `json:"file_path"`
		Offset, Limit *int
	}
	if err := decode(raw, &a); err != nil {
		return core.ToolResult{}, err
	}
	offset, limit := 1, 2000
	if a.Offset != nil {
		offset = *a.Offset
	}
	if a.Limit != nil {
		limit = *a.Limit
	}
	if offset < 1 {
		return core.ToolResult{}, errors.New("offset must be a positive integer")
	}
	if limit < 1 {
		return core.ToolResult{}, errors.New("limit must be a positive integer")
	}
	if limit > 2000 {
		return core.ToolResult{}, errors.New("limit must be less than or equal to 2000")
	}
	path, err := fileTarget(tc, a.Path)
	if err != nil {
		return core.ToolResult{}, err
	}
	resolved, e := filepath.EvalSymlinks(path)
	if e == nil {
		path = resolved
	}
	unlock := fileLock(path)
	defer unlock()
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		r.observe(tc.SessionID, path, fileObservation{})
		return core.ToolResult{}, err
	}
	if err != nil {
		return core.ToolResult{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return core.ToolResult{}, err
	}
	if !info.Mode().IsRegular() {
		return core.ToolResult{}, errors.New("path is not a regular file")
	}
	h := sha256.New()
	reader := bufio.NewReaderSize(io.TeeReader(f, h), 64<<10)
	value := readValue{Path: path, Offset: offset, Lines: []fileLine{}}
	var line []byte
	bytesUsed := 0
	capped := false
	consume := func() {
		value.TotalLines++
		if value.TotalLines < offset || len(value.Lines) >= limit || capped {
			line = nil
			return
		}
		s := strings.TrimSuffix(string(line), "\r")
		if !utf8.ValidString(s) {
			s = strings.ToValidUTF8(s, "�")
		}
		units := utf16.Encode([]rune(s))
		if len(units) > 2000 {
			s = string(utf16.Decode(units[:2000])) + "... (line truncated to 2000 chars)"
		}
		size := len(s)
		if len(value.Lines) > 0 {
			size++
		}
		if bytesUsed+size > 50*1024 {
			capped = true
		} else {
			value.Lines = append(value.Lines, fileLine{value.TotalLines, s})
			bytesUsed += size
		}
		line = nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return core.ToolResult{}, err
		}
		part, e := reader.ReadSlice('\n')
		hasNewline := len(part) > 0 && part[len(part)-1] == '\n'
		if hasNewline {
			part = part[:len(part)-1]
		}
		if len(line) < 8004 {
			take := 8004 - len(line)
			if len(part) < take {
				take = len(part)
			}
			line = append(line, part[:take]...)
		}
		if hasNewline {
			consume()
		}
		if e == io.EOF {
			if len(line) > 0 {
				consume()
			}
			break
		}
		if e != nil && e != bufio.ErrBufferFull {
			return core.ToolResult{}, e
		}
	}
	if offset > value.TotalLines && !(offset == 1 && value.TotalLines == 0) {
		return core.ToolResult{}, fmt.Errorf("offset %d is out of range for %q (%d lines)", offset, path, value.TotalLines)
	}
	after, err := f.Stat()
	if err != nil {
		return core.ToolResult{}, err
	}
	if info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		return core.ToolResult{}, errors.New("FS_STALE: file changed while reading; read it again")
	}
	r.observe(tc.SessionID, path, fileObservation{true, hex.EncodeToString(h.Sum(nil))})
	return result(value), nil
}

type fsMutationArgs struct {
	Path          string `json:"file_path"`
	Content       string `json:"content"`
	Old           string `json:"old_string"`
	New           string `json:"new_string"`
	ReplaceAll    bool   `json:"replace_all"`
	Sandbox       string `json:"sandbox_permissions"`
	Justification string `json:"justification"`
}

func mutationMode(ctx context.Context, tc core.ToolContext, name string, raw json.RawMessage, a fsMutationArgs) (core.ToolContext, error) {
	if a.Sandbox == "danger-full-access" {
		a.Sandbox = "full-access"
	}
	if (a.Sandbox == "") != (strings.TrimSpace(a.Justification) == "") {
		return tc, errors.New("sandbox_permissions and justification must be supplied together")
	}
	if a.Sandbox != "" {
		mode, err := requestedMode(ctx, tc, name, raw, a.Sandbox)
		if err != nil {
			return tc, err
		}
		tc.Permission = mode
	}
	return tc, nil
}
func (r *Registry) officialWrite(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	return r.officialMutate(ctx, tc, raw, false)
}
func (r *Registry) officialEdit(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	return r.officialMutate(ctx, tc, raw, true)
}
func (r *Registry) officialMutate(ctx context.Context, tc core.ToolContext, raw json.RawMessage, edit bool) (core.ToolResult, error) {
	var a fsMutationArgs
	if err := decode(raw, &a); err != nil {
		return core.ToolResult{}, err
	}
	name := "write"
	if edit {
		name = "edit"
		if a.Old == "" {
			return core.ToolResult{}, errors.New("old_string must be a non-empty string")
		}
		if a.Old == a.New {
			return core.ToolResult{}, errors.New("old_string and new_string must differ")
		}
	}
	tc, err := mutationMode(ctx, tc, name, raw, a)
	if err != nil {
		return core.ToolResult{}, err
	}
	path, err := fileTarget(tc, a.Path)
	if err != nil {
		return core.ToolResult{}, err
	}
	unlock := fileLock(path)
	defer unlock()
	if info, e := os.Lstat(path); e == nil && info.Mode()&os.ModeSymlink != 0 {
		return core.ToolResult{}, errors.New("refusing to overwrite a symbolic link")
	}
	prior, seen := r.observed(tc.SessionID, path)
	current, err := fileVersion(ctx, path)
	if err != nil {
		return core.ToolResult{}, err
	}
	if edit && !seen {
		return core.ToolResult{}, errors.New("FS_NOT_OBSERVED: read the file before editing it")
	}
	if edit && !prior.Present {
		return core.ToolResult{}, errors.New("FS_NOT_FOUND: cannot edit an absent file")
	}
	if prior.Present {
		if !current.Present || prior.Version != current.Version {
			return core.ToolResult{}, errors.New("FS_STALE: file changed since the last observation; read it again")
		}
	} else if current.Present {
		return core.ToolResult{}, errors.New("FS_NOT_OBSERVED: read the existing file before overwriting it")
	}
	var before any
	if current.Present {
		b, e := readText(path)
		if e != nil {
			return core.ToolResult{}, e
		}
		before = string(b)
	}
	content := a.Content
	if edit {
		old := before.(string)
		count := strings.Count(old, a.Old)
		if count == 0 {
			return core.ToolResult{}, errors.New("old_string was not found in the file")
		}
		if count > 1 && !a.ReplaceAll {
			return core.ToolResult{}, errors.New("old_string matches multiple locations; supply more context or replace_all")
		}
		n := 1
		if a.ReplaceAll {
			n = -1
		}
		content = strings.Replace(old, a.Old, a.New, n)
	}
	_, err = writeFile(ctx, tc, core.JSON(map[string]any{"path": path, "content": content}))
	if err != nil {
		return core.ToolResult{}, err
	}
	sum := sha256.Sum256([]byte(content))
	r.observe(tc.SessionID, path, fileObservation{true, hex.EncodeToString(sum[:])})
	value := map[string]any{"path": path, "before": before, "after": content}
	if !edit {
		operation := "create"
		if current.Present {
			operation = "update"
		}
		value["operation"] = operation
	}
	return result(value), nil
}

// RenderOfficialFileResult separates canonical tool values from model text.
// PTC receives canonical JSON; only the top-level presentation invokes this.
func RenderOfficialFileResult(name string, raw string) (string, bool) {
	if name == "read" {
		var v readValue
		if json.Unmarshal([]byte(raw), &v) != nil {
			return "", false
		}
		end := v.Offset - 1
		var lines []string
		for _, line := range v.Lines {
			lines = append(lines, fmt.Sprintf("%d: %s", line.Number, line.Text))
			end = line.Number
		}
		footer := fmt.Sprintf("(End of file - total %d lines)", v.TotalLines)
		if end < v.TotalLines {
			footer = fmt.Sprintf("(Showing lines %d-%d of %d. Use offset=%d to continue.)", v.Offset, end, v.TotalLines, end+1)
		}
		body := footer
		if len(lines) > 0 {
			body = strings.Join(lines, "\n") + "\n\n" + footer
		}
		return "<path>" + v.Path + "</path>\n<type>file</type>\n<content>\n" + body + "\n</content>", true
	}
	var v struct{ Path, Operation string }
	if json.Unmarshal([]byte(raw), &v) != nil {
		return "", false
	}
	switch name {
	case "write":
		verb := "Created"
		if v.Operation == "update" {
			verb = "Updated"
		}
		return "<path>" + v.Path + "</path>\n<type>file</type>\n<content>\n" + verb + " file\n</content>", true
	case "edit":
		return "The file " + v.Path + " has been updated successfully.", true
	}
	return "", false
}
