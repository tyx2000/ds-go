//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"dsgo/internal/core"
)

func TestSpecialFileReadHelper(t *testing.T) {
	path := os.Getenv("DSGO_TEST_FIFO")
	if path == "" {
		t.Skip("isolated helper")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var err error
	if os.Getenv("DSGO_TEST_READ_KIND") == "version" {
		_, err = fileVersion(ctx, path)
	} else {
		r := New()
		defer r.Close()
		name, args := "read", map[string]any{"file_path": path}
		if os.Getenv("DSGO_TEST_READ_KIND") == "legacy" {
			name, args = "read_file", map[string]any{"path": path}
		}
		_, err = r.Execute(ctx, core.ToolContext{SessionID: "fifo", CWD: filepath.Dir(path), Permission: "read-only"}, name, core.JSON(args))
	}
	if err == nil {
		t.Fatal("special file accepted")
	}
}

func TestNonregularReadsDoNotWaitForFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"official", "legacy", "version"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSpecialFileReadHelper$", "-test.timeout=3s")
			cmd.Env = append(os.Environ(), "DSGO_TEST_FIFO="+path, "DSGO_TEST_READ_KIND="+kind, "GORACE=atexit_sleep_ms=0")
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("FIFO read did not reject promptly: %v\n%s", ctx.Err(), output)
			}
			if err != nil {
				t.Fatalf("helper: %v\n%s", err, output)
			}
		})
	}
}
