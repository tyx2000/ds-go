package artifacts

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func putFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSpillRecoveryOwnerIntegrityAndUTF8(t *testing.T) {
	s := testStore(t)
	original := strings.Repeat("日志行🙂\n", 20000)
	retained, ref, err := s.Retain("owner", "turn", "run_shell", original, 50000)
	if err != nil || ref == nil {
		t.Fatalf("spill: %v %v", ref, err)
	}
	if len(retained) > 50000 || !utf8.ValidString(retained) || !strings.Contains(retained, ref.Path) {
		t.Fatal("invalid retained display")
	}
	var restored strings.Builder
	offset := 0
	for offset < len(original) {
		_, part, next, err := s.ReadSpill("owner", ref.ID, offset, 4096)
		if err != nil {
			t.Fatal(err)
		}
		if !utf8.ValidString(part) || next <= offset {
			t.Fatal("invalid UTF-8 pagination")
		}
		restored.WriteString(part)
		offset = next
	}
	if restored.String() != original {
		t.Fatal("recovery lost output")
	}
	if _, _, _, err = s.ReadSpill("other-owner", ref.ID, 0, 100); err == nil {
		t.Fatal("cross-owner read accepted")
	}
	if err = os.WriteFile(ref.Path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = s.ReadSpill("owner", ref.ID, 0, 100); err == nil {
		t.Fatal("corrupted spill accepted")
	}
	text, other, err := s.Retain("owner", "turn", "read_file", original, 50000)
	if err != nil || other != nil || text != original {
		t.Fatal("recovery tool was recursively spilled")
	}
}

func TestWorkspaceSnapshotCapturesNewStagedDeletedAndSymlinkWithoutGitMutation(t *testing.T) {
	s := testStore(t)
	cwd := t.TempDir()
	putFile(t, filepath.Join(cwd, "tracked.txt"), "alpha\nbeta\ngamma\n")
	putFile(t, filepath.Join(cwd, "deleted.txt"), "old\n")
	// An index is deliberately opaque to the recorder; the working tree is the
	// source of truth for pre-existing staged and untracked files alike.
	putFile(t, filepath.Join(cwd, ".git", "index"), "do not read or modify")
	putFile(t, filepath.Join(cwd, "staged.txt"), "staged before\n")
	outside := t.TempDir()
	putFile(t, filepath.Join(outside, "secret"), "outside secret")
	if err := os.Symlink(outside, filepath.Join(cwd, "outside")); err != nil {
		t.Fatal(err)
	}
	before, err := s.Capture(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := before.Files[".git/index"]; ok {
		t.Fatal("captured git internals")
	}
	if _, ok := before.Files["outside/secret"]; ok {
		t.Fatal("followed external symlink")
	}
	putFile(t, filepath.Join(cwd, "tracked.txt"), "alpha\nBETA\ngamma\n")
	putFile(t, filepath.Join(cwd, "staged.txt"), "staged after\n")
	putFile(t, filepath.Join(cwd, "untracked.txt"), "new\n")
	if err = os.Remove(filepath.Join(cwd, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	after, err := s.Capture(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := s.Compare(context.Background(), before, after, 2)
	if err != nil {
		t.Fatal(err)
	}
	if changes.Summary.Total != 4 || changes.Summary.Added != 3 || changes.Summary.Deleted != 3 {
		t.Fatalf("counts: %+v", changes.Summary)
	}
	if err = s.SaveChanges("session", "turn", changes); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(filepath.Dir(s.path))
	if err != nil {
		t.Fatal(err)
	}
	changes, err = reopened.Changes("session", "turn")
	if err != nil {
		t.Fatal(err)
	}
	var index int
	for i, f := range changes.Summary.Files {
		if f.Path == "tracked.txt" {
			index = i
		}
	}
	diff, err := reopened.Diff(context.Background(), changes, index)
	if err != nil {
		t.Fatal(err)
	}
	if diff.Kind != "text" || len(diff.Hunks) != 1 || strings.Join(diff.Hunks[0].Lines, "\n") != " alpha\n-beta\n+BETA\n gamma" {
		t.Fatalf("diff: %+v", diff)
	}
	indexBytes, err := os.ReadFile(filepath.Join(cwd, ".git", "index"))
	if err != nil || string(indexBytes) != "do not read or modify" {
		t.Fatal("git index changed")
	}
}

func TestWorkspaceBinaryOversizedAndCorruptedBlobs(t *testing.T) {
	s := testStore(t)
	cwd := t.TempDir()
	before, err := s.Capture(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(cwd, "binary"), "a\x00b")
	putFile(t, filepath.Join(cwd, "large"), strings.Repeat("x", MaxFileBytes+1))
	putFile(t, filepath.Join(cwd, "text"), "hello\n")
	after, err := s.Capture(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := s.Compare(context.Background(), before, after, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i, kind := range []string{"binary", "oversized", "text"} {
		d, err := s.Diff(context.Background(), changes, i)
		if err != nil || d.Kind != kind {
			t.Fatalf("diff %d: %+v %v", i, d, err)
		}
	}
	putFile(t, filepath.Join(s.path, "blobs", after.Files["text"].Digest), "corrupt")
	if _, err = s.Diff(context.Background(), changes, 2); err == nil {
		t.Fatal("corrupted snapshot blob accepted")
	}
}

func TestSnapshotCancellationAndStorageSymlinkDenied(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Capture(ctx, t.TempDir()); err == nil {
		t.Fatal("canceled snapshot succeeded")
	}
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "artifacts")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); err == nil {
		t.Fatal("symlink store accepted")
	}
}
