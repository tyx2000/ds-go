// Package reference provides advisory file and session candidates. Picking a
// candidate inserts ordinary prompt text; discovery never reads file contents.
package reference

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const DefaultFileMaxResults = 20
const DefaultFileMaxEntries = 50000

var DefaultFileExcludedDirectories = []string{".git", "node_modules", "dist", "build", "out", "coverage", "target", ".next", ".nuxt", ".turbo", ".venv", "__pycache__", ".pytest_cache", ".mypy_cache", ".gradle"}

type FileOptions struct {
	MaxResults, MaxEntries int
	ExcludedDirectories    []string // nil uses defaults; non-nil empty disables exclusions
}

type FileCandidate struct{ Path, Kind string }

type fileIndex struct {
	entries   []FileCandidate
	startedAt uint64
}
type fileGeneration struct {
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	entries   []FileCandidate
	err       error
	startedAt uint64
}

type Files struct {
	root          string
	options       FileOptions
	excluded      map[string]bool
	mu            sync.Mutex
	invalidations uint64
	settled       *fileIndex
	generation    *fileGeneration
	closed        bool
	scan          func(context.Context) ([]FileCandidate, error)
}

func NewFiles(root string, options FileOptions) (*Files, error) {
	if options.MaxResults == 0 {
		options.MaxResults = DefaultFileMaxResults
	}
	if options.MaxEntries == 0 {
		options.MaxEntries = DefaultFileMaxEntries
	}
	if options.MaxResults < 1 || options.MaxEntries < 1 || uint64(options.MaxResults) > 9007199254740991 || uint64(options.MaxEntries) > 9007199254740991 {
		return nil, errors.New("file search limits must be positive safe integers")
	}
	if options.ExcludedDirectories == nil {
		options.ExcludedDirectories = append([]string(nil), DefaultFileExcludedDirectories...)
	} else {
		options.ExcludedDirectories = append([]string{}, options.ExcludedDirectories...)
	}
	excluded := map[string]bool{}
	for _, name := range options.ExcludedDirectories {
		if name == "" || strings.ContainsAny(name, "/\\") {
			return nil, errors.New("file search exclusions must be non-empty directory basenames")
		}
		excluded[name] = true
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	f := &Files{root: abs, options: options, excluded: excluded}
	f.scan = f.walk
	return f, nil
}

func (f *Files) List(ctx context.Context, rawQuery string) ([]FileCandidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	if closed {
		return []FileCandidate{}, nil
	}
	query := strings.ReplaceAll(rawQuery, "\\", "/")
	slash := strings.LastIndex(query, "/")
	if query == "" || slash >= 0 {
		directory, fragment := "", ""
		if slash >= 0 {
			directory, fragment = query[:slash+1], query[slash+1:]
		}
		return f.listDirectory(ctx, directory, fragment)
	}
	entries, err := f.indexFor(ctx)
	if err != nil {
		return nil, err
	}
	visible := make([]FileCandidate, 0, len(entries))
	for _, candidate := range entries {
		if visibleFile(candidate.Path, query) {
			visible = append(visible, candidate)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return rankFiles(visible, query, f.options.MaxResults), nil
}

func (f *Files) Invalidate() { f.mu.Lock(); f.invalidations++; f.mu.Unlock() }
func (f *Files) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		if f.generation != nil {
			f.generation.cancel()
		}
		f.generation = nil
		f.settled = nil
	}
	return nil
}

func (f *Files) indexFor(ctx context.Context) ([]FileCandidate, error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return []FileCandidate{}, nil
	}
	if f.settled != nil {
		entries := f.settled.entries
		if f.settled.startedAt < f.invalidations {
			f.ensureIndexLocked()
		}
		f.mu.Unlock()
		return entries, nil
	}
	generation := f.ensureIndexLocked()
	f.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-generation.done:
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return generation.entries, generation.err
}

func (f *Files) ensureIndexLocked() *fileGeneration {
	if f.generation != nil {
		return f.generation
	}
	ctx, cancel := context.WithCancel(context.Background())
	generation := &fileGeneration{ctx: ctx, cancel: cancel, done: make(chan struct{}), startedAt: f.invalidations}
	f.generation = generation
	scan := f.scan
	go func() {
		entries, err := scan(ctx)
		if err == nil {
			err = ctx.Err()
		}
		generation.entries, generation.err = entries, err
		f.mu.Lock()
		if f.generation == generation {
			f.generation = nil
			if !f.closed && err == nil {
				f.settled = &fileIndex{entries: entries, startedAt: generation.startedAt}
			}
		}
		f.mu.Unlock()
		cancel()
		close(generation.done)
	}()
	return generation
}

func (f *Files) openRoot() (*os.Root, error) {
	// The final /. requires the selected root to be a directory before open;
	// replacing it with a FIFO cannot block the completion worker.
	return os.OpenRoot(f.root + string(filepath.Separator) + ".")
}

func (f *Files) walk(ctx context.Context) ([]FileCandidate, error) {
	root, err := f.openRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	indexed := make([]FileCandidate, 0, min(f.options.MaxEntries, 1024))
	directories := []string{""}
	for cursor := 0; cursor < len(directories) && len(indexed) < f.options.MaxEntries; cursor++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		directory := directories[cursor]
		entries, err := readFileDirectory(ctx, root, directory, cursor == 0)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			path := entry.Name()
			if directory != "" {
				path = directory + "/" + path
			}
			if entry.IsDir() {
				if f.excluded[entry.Name()] {
					continue
				}
				indexed = append(indexed, FileCandidate{Path: path, Kind: "directory"})
				directories = append(directories, path)
			} else if entry.Type().IsRegular() {
				indexed = append(indexed, FileCandidate{Path: path, Kind: "file"})
			}
			if len(indexed) >= f.options.MaxEntries {
				break
			}
		}
	}
	return indexed, nil
}

func (f *Files) listDirectory(ctx context.Context, displayDirectory, fragment string) ([]FileCandidate, error) {
	for _, part := range strings.Split(displayDirectory, "/") {
		if f.excluded[part] {
			return []FileCandidate{}, nil
		}
	}
	absolute := filepath.FromSlash(displayDirectory)
	if !filepath.IsAbs(absolute) {
		absolute = filepath.Join(f.root, absolute)
	}
	relative, err := filepath.Rel(f.root, filepath.Clean(absolute))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return []FileCandidate{}, nil
	}
	root, err := f.openRoot()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return []FileCandidate{}, nil
	}
	defer root.Close()
	entries, err := readFileDirectory(ctx, root, filepath.ToSlash(relative), false)
	if err != nil {
		return nil, err
	}
	candidates := make([]FileCandidate, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.HasPrefix(entry.Name(), ".") && !strings.HasPrefix(fragment, ".") {
			continue
		}
		if entry.IsDir() {
			if !f.excluded[entry.Name()] {
				candidates = append(candidates, FileCandidate{Path: displayDirectory + entry.Name(), Kind: "directory"})
			}
		} else if entry.Type().IsRegular() {
			candidates = append(candidates, FileCandidate{Path: displayDirectory + entry.Name(), Kind: "file"})
		}
	}
	return rankFiles(candidates, fragment, f.options.MaxResults), nil
}

func readFileDirectory(ctx context.Context, root *os.Root, directory string, strict bool) ([]os.DirEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name := "."
	if directory != "" && directory != "." {
		for _, part := range strings.Split(directory, "/") {
			if part == "" || part == "." {
				continue
			}
			if name == "." {
				name = part
			} else {
				name += "/" + part
			}
			info, err := root.Lstat(filepath.FromSlash(name))
			if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return []os.DirEntry{}, nil
			}
		}
	}
	file, err := openFileDirectory(root, filepath.FromSlash(name))
	if err == nil {
		defer file.Close()
	}
	var entries []os.DirEntry
	if err == nil {
		entries, err = file.ReadDir(-1)
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if err != nil {
		if strict {
			return nil, err
		}
		return []os.DirEntry{}, nil
	}
	sort.Slice(entries, func(i, j int) bool { return compareFileText(entries[i].Name(), entries[j].Name()) < 0 })
	return entries, nil
}

func visibleFile(path, query string) bool {
	if strings.HasPrefix(query, ".") || strings.Contains(query, "/.") {
		return true
	}
	for _, part := range strings.Split(path, "/") {
		if strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}
