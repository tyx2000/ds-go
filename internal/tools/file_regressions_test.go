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
	for _, name := range []string{"write", "edit"} {
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
