package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewSandboxLauncherIgnoresHarnessPATH(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "sandbox-exec")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	workspace := t.TempDir()
	got, err := trustedSandboxLauncher("sandbox-exec", workspace)
	if err == nil && strings.HasPrefix(got, dir+string(filepath.Separator)) {
		t.Fatalf("launcher was resolved from PATH: %s", got)
	}
}
