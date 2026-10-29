package state

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestReplayKeyPersistsAcrossConcurrentStores(t *testing.T) {
	root := t.TempDir()
	const count = 8
	keys := make([][]byte, count)
	errs := make([]error, count)
	var wg sync.WaitGroup
	for i := range keys {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := Open(root)
			if err == nil {
				keys[i], err = s.ReplayKey()
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i := range keys {
		if errs[i] != nil || len(keys[i]) != 32 || !bytes.Equal(keys[0], keys[i]) {
			t.Fatalf("key %d: length=%d error=%v", i, len(keys[i]), errs[i])
		}
	}
	info, err := os.Lstat(filepath.Join(root, replayKeyName))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private key permissions: %v %v", info, err)
	}
	entries, _ := os.ReadDir(root)
	for _, entry := range entries {
		if len(entry.Name()) > len(replayKeyName) && entry.Name()[:len(replayKeyName)] == replayKeyName {
			t.Fatal("temporary key was not removed")
		}
	}
	other, _ := Open(t.TempDir())
	otherKey, err := other.ReplayKey()
	if err != nil || bytes.Equal(keys[0], otherKey) {
		t.Fatal("separate state roots must have separate identity keys")
	}
}

func TestReplayKeyRejectsInvalidOrLinkedFile(t *testing.T) {
	for _, kind := range []string{"short", "long", "directory", "symlink", "public"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := Open(t.TempDir())
			path := filepath.Join(s.Root(), replayKeyName)
			var err error
			switch kind {
			case "directory":
				err = os.Mkdir(path, 0700)
			case "symlink":
				target := filepath.Join(t.TempDir(), "target")
				err = os.WriteFile(target, make([]byte, 32), 0600)
				if err == nil {
					err = os.Symlink(target, path)
				}
			default:
				n := 32
				if kind == "short" {
					n--
				}
				if kind == "long" {
					n++
				}
				err = os.WriteFile(path, make([]byte, n), 0600)
				if err == nil && kind == "public" {
					err = os.Chmod(path, 0644)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.ReplayKey(); err == nil {
				t.Fatal("invalid key accepted or replaced")
			}
		})
	}
}
