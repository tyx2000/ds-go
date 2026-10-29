package reference

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func fileFixture(t *testing.T, root, path string) {
	t.Helper()
	absolute := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolute, []byte("contents are not completion data"), 0600); err != nil {
		t.Fatal(err)
	}
}
func candidatePaths(values []FileCandidate) []string {
	out := make([]string, len(values))
	for n, value := range values {
		out[n] = value.Path
	}
	return out
}
func listFiles(t *testing.T, f *Files, query string) []FileCandidate {
	t.Helper()
	values, err := f.List(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	return values
}
func waitFileGeneration(t *testing.T, f *Files) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		f.mu.Lock()
		generation := f.generation
		f.mu.Unlock()
		if generation == nil {
			return
		}
		select {
		case <-generation.done:
		case <-time.After(time.Until(deadline)):
			t.Fatal("file index did not settle")
		}
	}
}

func TestFilesLiveDirectoriesHiddenExclusionsAndSymlinks(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"alpha.txt", ".hidden", "lib/test.go", "lib/oldtest.go", "lib/.env", ".cache/hidden-needle", "dist/built.js", "node_modules/dependency.js"} {
		fileFixture(t, root, path)
	}
	outside := t.TempDir()
	fileFixture(t, outside, "outside-secret")
	if err := os.Symlink(outside, filepath.Join(root, "external")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "lib"), filepath.Join(root, "internal")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "alpha.txt"), filepath.Join(root, "file-link")); err != nil {
		t.Fatal(err)
	}
	f, err := NewFiles(root, FileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got := candidatePaths(listFiles(t, f, "")); !reflect.DeepEqual(got, []string{"lib", "alpha.txt"}) {
		t.Fatalf("root live rows: %v", got)
	}
	if got := candidatePaths(listFiles(t, f, "lib/te")); !reflect.DeepEqual(got, []string{"lib/test.go", "lib/oldtest.go"}) {
		t.Fatalf("directory ranking: %v", got)
	}
	fileFixture(t, root, "lib/testing.go")
	if got := candidatePaths(listFiles(t, f, "lib\\te")); !reflect.DeepEqual(got, []string{"lib/test.go", "lib/testing.go", "lib/oldtest.go"}) {
		t.Fatalf("live update/normalized separator: %v", got)
	}
	for _, query := range []string{"external/", "internal/", "../", filepath.ToSlash(outside) + "/", "dist/", "node_modules/"} {
		if got := listFiles(t, f, query); len(got) != 0 {
			t.Fatalf("unsafe/excluded %q: %v", query, got)
		}
	}
	if got := listFiles(t, f, "needle"); len(got) != 0 {
		t.Fatalf("hidden paths exposed by global query: %v", got)
	}
	if got := candidatePaths(listFiles(t, f, ".env")); !reflect.DeepEqual(got, []string{"lib/.env"}) {
		t.Fatalf("explicit hidden query: %v", got)
	}
	if got := candidatePaths(listFiles(t, f, "lib/.")); len(got) == 0 || got[0] != "lib/.env" {
		t.Fatalf("hidden live rows: %v", got)
	}
}

func TestFilesBoundedBreadthFirstIndexAndOptions(t *testing.T) {
	root := t.TempDir()
	fileFixture(t, root, "a/deep-a")
	fileFixture(t, root, "b/deep-b")
	fileFixture(t, root, "c.txt")
	f, err := NewFiles(root, FileOptions{MaxEntries: 3, MaxResults: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got := listFiles(t, f, "deep"); len(got) != 0 {
		t.Fatalf("scan exceeded breadth-first limit: %v", got)
	}
	f.mu.Lock()
	entries := append([]FileCandidate(nil), f.settled.entries...)
	f.mu.Unlock()
	if got := candidatePaths(entries); !reflect.DeepEqual(got, []string{"a", "b", "c.txt"}) {
		t.Fatalf("bounded BFS order: %v", got)
	}
	if got := listFiles(t, f, ""); len(got) != 1 || got[0].Path != "a" {
		t.Fatalf("result cap: %v", got)
	}
	for _, options := range []FileOptions{{MaxResults: -1}, {MaxEntries: -1}, {ExcludedDirectories: []string{""}}, {ExcludedDirectories: []string{"a/b"}}, {ExcludedDirectories: []string{"a\\b"}}} {
		if _, err := NewFiles(root, options); err == nil {
			t.Fatalf("accepted bad options %+v", options)
		}
	}
	unfiltered, err := NewFiles(root, FileOptions{ExcludedDirectories: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	defer unfiltered.Close()
	fileFixture(t, root, "dist/output")
	if got := listFiles(t, unfiltered, "dist/"); len(got) != 1 {
		t.Fatalf("explicit empty exclusions ignored: %v", got)
	}
}

func TestFileRankingMatchesOfficialScoresAndUTF16Ties(t *testing.T) {
	candidates := []FileCandidate{{Path: "f/o/o.txt", Kind: "file"}, {Path: "foo/path.txt", Kind: "file"}, {Path: "myfoo", Kind: "file"}, {Path: "foo-long", Kind: "file"}, {Path: "foo", Kind: "file"}, {Path: "foo", Kind: "directory"}, {Path: "other", Kind: "file"}}
	if got := candidatePaths(rankFiles(candidates, "foo", 20)); !reflect.DeepEqual(got, []string{"foo", "foo", "foo-long", "myfoo", "foo/path.txt", "f/o/o.txt"}) {
		t.Fatalf("official score order: %v", got)
	}
	values := rankFiles([]FileCandidate{{Path: "\ue000", Kind: "file"}, {Path: "😀", Kind: "file"}, {Path: "a", Kind: "directory"}}, "", 20)
	if got := candidatePaths(values); !reflect.DeepEqual(got, []string{"a", "😀", "\ue000"}) {
		t.Fatalf("UTF16 lexical ordering: %v", got)
	}
	if got := candidatePaths(rankFiles([]FileCandidate{{Path: "😀f", Kind: "file"}, {Path: "xxf", Kind: "file"}}, "f", 20)); !reflect.DeepEqual(got, []string{"xxf", "😀f"}) {
		t.Fatalf("UTF16 length tie: %v", got)
	}
	if score, ok := scoreFile(FileCandidate{Kind: "file"}, "😀abc", "😀b"); !ok || score != 398 {
		t.Fatalf("UTF16 subsequence score %d %v", score, ok)
	}
	if got := rankFiles([]FileCandidate{{Path: "İ.txt", Kind: "file"}}, "i\u0307", 20); len(got) != 1 {
		t.Fatalf("Unicode lower-case expansion lost: %v", got)
	}
}

func TestFilesCancelledCallerDoesNotCancelSharedScan(t *testing.T) {
	f, err := NewFiles(t.TempDir(), FileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	started, release := make(chan struct{}), make(chan struct{})
	var scans atomic.Int64
	f.scan = func(ctx context.Context) ([]FileCandidate, error) {
		scans.Add(1)
		close(started)
		select {
		case <-release:
			return []FileCandidate{{Path: "shared", Kind: "file"}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := f.List(ctx, "share"); first <- err }()
	<-started
	second := make(chan []FileCandidate, 1)
	go func() { values, _ := f.List(context.Background(), "share"); second <- values }()
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation: %v", err)
	}
	close(release)
	select {
	case values := <-second:
		if len(values) != 1 || values[0].Path != "shared" {
			t.Fatalf("shared scan lost: %v", values)
		}
	case <-time.After(time.Second):
		t.Fatal("shared scan was cancelled")
	}
	if scans.Load() != 1 {
		t.Fatalf("duplicate shared traversals: %d", scans.Load())
	}
}

func TestFilesStaleIndexAnswersWhileRefreshingAndFailureRetainsIt(t *testing.T) {
	f, err := NewFiles(t.TempDir(), FileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.scan = func(context.Context) ([]FileCandidate, error) {
		return []FileCandidate{{Path: "old", Kind: "file"}}, nil
	}
	if got := listFiles(t, f, "old"); len(got) != 1 {
		t.Fatal(got)
	}
	started, release := make(chan struct{}), make(chan struct{})
	f.scan = func(ctx context.Context) ([]FileCandidate, error) {
		close(started)
		select {
		case <-release:
			return []FileCandidate{{Path: "new", Kind: "file"}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.Invalidate()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if got, err := f.List(ctx, "old"); err != nil || len(got) != 1 {
		t.Fatalf("stale query waited on refresh: %v %v", got, err)
	}
	<-started
	close(release)
	waitFileGeneration(t, f)
	if got := listFiles(t, f, "new"); len(got) != 1 {
		t.Fatalf("replacement not published: %v", got)
	}
	f.scan = func(context.Context) ([]FileCandidate, error) { return nil, errors.New("root unreadable") }
	f.Invalidate()
	if got := listFiles(t, f, "new"); len(got) != 1 {
		t.Fatal(got)
	}
	waitFileGeneration(t, f)
	f.mu.Lock()
	retained := candidatePaths(f.settled.entries)
	stale := f.settled.startedAt < f.invalidations
	f.mu.Unlock()
	if !stale || !reflect.DeepEqual(retained, []string{"new"}) {
		t.Fatalf("failed refresh overwrote good index: %v stale=%v", retained, stale)
	}
}

func TestFilesCloseCancelsScanAndReturnsNoLaterCandidates(t *testing.T) {
	f, err := NewFiles(t.TempDir(), FileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	started, stopped := make(chan struct{}), make(chan struct{})
	f.scan = func(ctx context.Context) ([]FileCandidate, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}
	result := make(chan error, 1)
	go func() { _, err := f.List(context.Background(), "file"); result <- err }()
	<-started
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("close did not cancel traversal")
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("closed scan: %v", err)
	}
	if got := listFiles(t, f, ""); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestFilesUnreadableRootRefreshPreservesIndexAndRetries(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	moved := filepath.Join(parent, "moved")
	fileFixture(t, root, "old.txt")
	f, err := NewFiles(root, FileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if values := listFiles(t, f, "old"); len(values) != 1 {
		t.Fatal(values)
	}
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(moved, root)
	f.Invalidate()
	if values := listFiles(t, f, "old"); len(values) != 1 {
		t.Fatal(values)
	}
	waitFileGeneration(t, f)
	f.mu.Lock()
	retained := candidatePaths(f.settled.entries)
	f.mu.Unlock()
	if !reflect.DeepEqual(retained, []string{"old.txt"}) {
		t.Fatalf("missing root erased old index: %v", retained)
	}
	if err := os.Rename(moved, root); err != nil {
		t.Fatal(err)
	}
	fileFixture(t, root, "new.txt")
	listFiles(t, f, "old")
	waitFileGeneration(t, f)
	if values := listFiles(t, f, "new"); len(values) != 1 {
		t.Fatalf("failed refresh was not retried: %v", values)
	}
}

func TestFileTokenGrammarAndMentionFormatting(t *testing.T) {
	for _, test := range []struct {
		line          string
		cursor        int
		prefix, query string
		quoted, ok    bool
	}{
		{"@", 1, "@", "", false, true}, {"name@example.com", 16, "", "", false, false},
		{"你好 @lib/file tail", 12, "@lib/file", "lib/file", false, true},
		{"see @\"my file", 13, "@\"my file", "my file", true, true},
		{"@\"closed\"", 9, "@\"closed\"", "\"closed\"", false, true},
		{"x\u0085@file", 7, "", "", false, false}, {"x\u00a0@file", 7, "@file", "file", false, true},
	} {
		token, ok := ActiveAtToken(test.line, test.cursor)
		if ok != test.ok || ok && (token.Prefix != test.prefix || token.Query != test.query || token.Quoted != test.quoted || token.End-token.Start != len([]rune(token.Prefix))) {
			t.Fatalf("token %q@%d: %+v %v", test.line, test.cursor, token, ok)
		}
	}
	for _, test := range []struct {
		path, kind string
		preserve   bool
		want       string
		ok         bool
	}{
		{"src", "directory", false, "@src/", true}, {"my dir", "directory", false, "@\"my dir/", true}, {"my file", "file", false, "@\"my file\"", true}, {"plain", "file", true, "@\"plain\"", true}, {"bad\nfile", "file", false, "", false}, {"bad\"file", "file", false, "", false},
	} {
		got, ok := FormatFileMention(test.path, test.kind, test.preserve)
		if got != test.want || ok != test.ok {
			t.Fatalf("format %q: %q %v", test.path, got, ok)
		}
	}
}
