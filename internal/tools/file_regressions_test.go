package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dsgo/internal/core"
)

func TestMutationRechecksVersionAfterApproval(t *testing.T) {
	for _, name := range []string{"write", "edit", "write_file", "edit_file"} {
		t.Run(name, func(t *testing.T) {
			r := New()
			defer r.Close()
			tc := toolContext(t)
			path := filepath.Join(tc.CWD, "existing")
			if err := os.WriteFile(path, []byte("observed"), 0600); err != nil {
				t.Fatal(err)
			}
			call(t, r, tc, "read", map[string]any{"file_path": path})
			tc.Permission, tc.ApprovalPolicy = "read-only", "on-request"
			tc.Approve = func(context.Context, core.Approval) (bool, error) {
				return true, os.WriteFile(path, []byte("external change during approval"), 0600)
			}
			args := map[string]any{"file_path": path, "content": "model overwrite"}
			switch name {
			case "edit":
				args = map[string]any{"file_path": path, "old_string": "observed", "new_string": "model overwrite"}
			case "write_file":
				args = map[string]any{"path": path, "content": "model overwrite"}
			case "edit_file":
				args = map[string]any{"path": path, "old_text": "observed", "new_text": "model overwrite"}
			}
			_, err := r.Execute(context.Background(), tc, name, core.JSON(args))
			if err == nil || !strings.Contains(err.Error(), "FS_STALE") {
				t.Fatalf("expected stale after approval, got %v", err)
			}
			data, _ := os.ReadFile(path)
			if string(data) != "external change during approval" {
				t.Fatalf("external edit overwritten: %q", data)
			}
		})
	}
}

func TestMutationDoesNotClobberFileCreatedDuringApproval(t *testing.T) {
	r := New()
	defer r.Close()
	tc := toolContext(t)
	path := filepath.Join(tc.CWD, "new")
	tc.Permission, tc.ApprovalPolicy = "read-only", "on-request"
	tc.Approve = func(context.Context, core.Approval) (bool, error) {
		return true, os.WriteFile(path, []byte("created externally"), 0600)
	}
	_, err := r.Execute(context.Background(), tc, "write", core.JSON(map[string]any{"file_path": path, "content": "clobber"}))
	if err == nil {
		t.Fatal("file created during approval was overwritten")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "created externally" {
		t.Fatalf("external file lost: %q", data)
	}
}

func TestLegacyMutationsShareObservationPolicy(t *testing.T) {
	r := New()
	defer r.Close()
	tc := toolContext(t)
	path := filepath.Join(tc.CWD, "existing")
	os.WriteFile(path, []byte("before"), 0600)
	for _, name := range []string{"write_file", "edit_file"} {
		args := map[string]any{"path": path, "content": "bad"}
		if name == "edit_file" {
			args = map[string]any{"path": path, "old_text": "before", "new_text": "bad"}
		}
		if _, err := r.Execute(context.Background(), tc, name, core.JSON(args)); err == nil || !strings.Contains(err.Error(), "FS_NOT_OBSERVED") {
			t.Fatalf("%s bypassed observation: %v", name, err)
		}
	}
	call(t, r, tc, "read_file", map[string]any{"path": path})
	call(t, r, tc, "edit", map[string]any{"file_path": path, "old_string": "before", "new_string": "after"})
	call(t, r, tc, "write_file", map[string]any{"path": path, "content": "again"})
	os.WriteFile(path, []byte("external"), 0600)
	if _, err := r.Execute(context.Background(), tc, "edit_file", core.JSON(map[string]any{"path": path, "old_text": "external", "new_text": "bad"})); err == nil || !strings.Contains(err.Error(), "FS_STALE") {
		t.Fatalf("legacy edit accepted stale observation: %v", err)
	}
}
