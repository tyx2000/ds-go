//go:build darwin || linux

package reference

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestFilesRejectsFIFORootWithoutBlocking(t *testing.T) {
	root := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(root, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := NewFiles(root, FileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, err := f.List(ctx, "anything"); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("FIFO root was opened as a stream: %v", err)
	}
}
