package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dsgo/internal/core"
)

// Earlier dsgo transcripts used different argument and output fields. Keep
// those wire shapes at the edge, with every read and mutation using the same
// owner observations, permission boundary and guarded commit as official tools.
func (r *Registry) legacyRead(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	var a struct {
		Path          string
		Offset, Limit int
	}
	if err := decode(raw, &a); err != nil {
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
	path, err := fileTarget(tc, a.Path)
	if err != nil {
		return core.ToolResult{}, err
	}
	if resolved, e := filepath.EvalSymlinks(path); e == nil {
		path = resolved
	}
	unlock := fileLock(path)
	defer unlock()
	root, rel, err := openAncestor(path)
	if err != nil {
		return core.ToolResult{}, err
	}
	defer root.Close()
	data, observed, _, err := readSnapshotAt(ctx, root, rel)
	if err != nil {
		return core.ToolResult{}, err
	}
	if !observed.Present {
		r.observe(tc.SessionID, path, observed)
		return core.ToolResult{}, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if a.Offset > len(lines) {
		a.Offset = len(lines)
	}
	end := min(a.Offset+a.Limit, len(lines))
	var text strings.Builder
	next := a.Offset
	for i := a.Offset; i < end; i++ {
		line := fmt.Sprintf("%6d\t%s\n", i+1, lines[i])
		if text.Len()+len(line) > maxOutputBytes {
			break
		}
		text.WriteString(line)
		next = i + 1
	}
	r.observe(tc.SessionID, path, observed)
	return result(map[string]any{"path": path, "offset": a.Offset, "next_offset": next, "total_lines": len(lines), "truncated": next < len(lines), "content": text.String()}), nil
}

func (r *Registry) legacyWrite(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	var a struct{ Path, Content string }
	if err := decode(raw, &a); err != nil {
		return core.ToolResult{}, err
	}
	res, err := r.officialWrite(ctx, tc, core.JSON(map[string]any{"file_path": a.Path, "content": a.Content}))
	if err != nil {
		return core.ToolResult{}, err
	}
	var v struct{ Path string }
	if err := json.Unmarshal([]byte(res.Content), &v); err != nil {
		return core.ToolResult{}, err
	}
	return result(map[string]any{"path": v.Path, "bytes": len(a.Content)}), nil
}

func (r *Registry) legacyEdit(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	var a struct {
		Path string
		Old  string `json:"old_text"`
		New  string `json:"new_text"`
		All  bool   `json:"replace_all"`
	}
	if err := decode(raw, &a); err != nil {
		return core.ToolResult{}, err
	}
	res, err := r.officialEdit(ctx, tc, core.JSON(map[string]any{"file_path": a.Path, "old_string": a.Old, "new_string": a.New, "replace_all": a.All}))
	if err != nil {
		return core.ToolResult{}, err
	}
	var v struct{ Path, Before, After string }
	if err := json.Unmarshal([]byte(res.Content), &v); err != nil {
		return core.ToolResult{}, err
	}
	count := strings.Count(v.Before, a.Old)
	if !a.All {
		count = 1
	}
	return result(map[string]any{"path": v.Path, "replacements": count, "bytes": len(v.After)}), nil
}
