package state

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"dsgo/internal/core"
)

var ErrBusy = errors.New("session is already running in another dsgo process")
var validID = regexp.MustCompile(`^[a-f0-9]{32}$`)

const maxEventBytes = 32 << 20

type Store struct {
	root string
	mu   sync.Mutex
}

func Open(root string) (*Store, error) {
	if root == "" {
		root = DefaultRoot()
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := secureDir(absolute); err != nil {
		return nil, err
	}
	if err := secureDir(filepath.Join(absolute, "sessions")); err != nil {
		return nil, err
	}
	return &Store{root: absolute}, nil
}

func (s *Store) Root() string { return s.root }

func secureDir(path string) error {
	if err := rejectSymlink(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("state path is not a directory: %s", path)
	}
	return os.Chmod(path, 0700)
}

func rejectSymlink(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("state path must not be a symlink: %s", path)
	}
	return nil
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *Store) dir(id string) (string, error) {
	if !validID.MatchString(id) {
		return "", errors.New("invalid session ID: expected 32 lowercase hexadecimal characters")
	}
	dir := filepath.Join(s.root, "sessions", id)
	if err := rejectSymlink(dir); err != nil {
		return "", err
	}
	return dir, nil
}

func openLock(path string) (*os.File, error) {
	if err := rejectSymlink(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (s *Store) withLock(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := openLock(filepath.Join(s.root, ".lock"))
	if err != nil {
		return err
	}
	defer f.Close()
	if err := lockFile(context.Background(), f, true); err != nil {
		return err
	}
	defer unlockFile(f)
	return fn()
}

// AcquireSession reserves one running turn without blocking readers or Append.
// A process crash releases this OS lock; no PID or stale-lock guessing is used.
func (s *Store) AcquireSession(ctx context.Context, id string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var f *os.File
	err := s.withLock(func() error {
		if _, err := s.get(id); err != nil {
			return err
		}
		dir, err := s.dir(id)
		if err != nil {
			return err
		}
		f, err = openLock(filepath.Join(dir, ".turn.lock"))
		if err != nil {
			return err
		}
		if err := lockFile(ctx, f, false); err != nil {
			f.Close()
			f = nil
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { _ = unlockFile(f); _ = f.Close() }) }, nil
}

func atomicWrite(path string, data []byte) error {
	if err := rejectSymlink(path); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".write-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *Store) create(cwd, title string) (core.Session, error) {
	return s.createWithParent(cwd, title, "")
}

func (s *Store) createWithParent(cwd, title, parentID string) (core.Session, error) {
	id, err := newID()
	if err != nil {
		return core.Session{}, err
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return core.Session{}, err
	}
	if title == "" {
		title = "新会话"
	}
	now := time.Now().UTC()
	session := core.Session{ID: id, Title: title, CWD: cwd, ParentID: parentID, CreatedAt: now, UpdatedAt: now, Delegated: parentID != ""}
	dir, _ := s.dir(id)
	if err := secureDir(dir); err != nil {
		return core.Session{}, err
	}
	if err := s.update(session); err != nil {
		os.RemoveAll(dir)
		return core.Session{}, err
	}
	if err := atomicWrite(filepath.Join(dir, "events.jsonl"), nil); err != nil {
		os.RemoveAll(dir)
		return core.Session{}, err
	}
	return session, nil
}

func (s *Store) Create(cwd, title string) (session core.Session, err error) {
	err = s.withLock(func() error { session, err = s.create(cwd, title); return err })
	return
}

// CreateChild creates a fresh session whose ancestry is part of its initial
// metadata. It never copies parent history or relaxes Update's identity checks.
func (s *Store) CreateChild(cwd, title, parentID string) (session core.Session, err error) {
	err = s.withLock(func() error {
		if _, err := s.get(parentID); err != nil {
			return err
		}
		session, err = s.createWithParent(cwd, title, parentID)
		return err
	})
	return
}

func (s *Store) get(id string) (core.Session, error) {
	dir, err := s.dir(id)
	if err != nil {
		return core.Session{}, err
	}
	path := filepath.Join(dir, "meta.json")
	if err := rejectSymlink(path); err != nil {
		return core.Session{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return core.Session{}, err
	}
	var session core.Session
	if err := json.Unmarshal(b, &session); err != nil {
		return session, fmt.Errorf("decode session %s metadata: %w", id, err)
	}
	if session.ID != id {
		return session, errors.New("session metadata ID mismatch")
	}
	return session, nil
}

func (s *Store) Get(id string) (session core.Session, err error) {
	err = s.withLock(func() error { session, err = s.get(id); return err })
	return
}

func (s *Store) update(session core.Session) error {
	dir, err := s.dir(session.ID)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, "meta.json"), append(b, '\n'))
}

func (s *Store) Update(session core.Session) error {
	return s.withLock(func() error {
		old, err := s.get(session.ID)
		if err != nil {
			return err
		}
		// Identity and ancestry are immutable after creation.
		if old.ParentID != session.ParentID || !old.CreatedAt.Equal(session.CreatedAt) {
			return errors.New("cannot change session ancestry or creation time")
		}
		session.UpdatedAt = time.Now().UTC()
		return s.update(session)
	})
}

func (s *Store) List(includeArchived bool) (sessions []core.Session, err error) {
	err = s.withLock(func() error {
		entries, err := os.ReadDir(filepath.Join(s.root, "sessions"))
		if err != nil {
			return err
		}
		for _, ent := range entries {
			if !validID.MatchString(ent.Name()) {
				continue
			}
			if !ent.IsDir() {
				return fmt.Errorf("invalid session directory %s", ent.Name())
			}
			session, err := s.get(ent.Name())
			if err != nil {
				return err
			}
			if includeArchived || !session.Archived {
				sessions = append(sessions, session)
			}
		}
		sort.Slice(sessions, func(i, j int) bool { return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt) })
		return nil
	})
	if sessions == nil {
		sessions = []core.Session{}
	}
	return
}

// readEvents returns the valid prefix and its byte boundary. A torn final line
// can be discarded, but corruption in any committed newline-terminated record
// is always an error. A valid final JSON record missing only its newline stays.
func readEvents(path string) (events []core.Event, validBytes int64, needsNewline bool, err error) {
	if err := rejectSymlink(path); err != nil {
		return nil, 0, false, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, false, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	var offset int64
	for {
		var line []byte
		for {
			fragment, readErr := r.ReadSlice('\n')
			line = append(line, fragment...)
			if len(line) > maxEventBytes {
				return nil, 0, false, fmt.Errorf("event exceeds %d bytes at offset %d", maxEventBytes, offset)
			}
			if errors.Is(readErr, bufio.ErrBufferFull) {
				continue
			}
			err = readErr
			break
		}
		if len(line) == 0 && errors.Is(err, io.EOF) {
			return events, offset, false, nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, 0, false, err
		}
		var ev core.Event
		decodeErr := json.Unmarshal(bytes.TrimSpace(line), &ev)
		if decodeErr != nil {
			if errors.Is(err, io.EOF) {
				return events, offset, false, nil
			}
			return nil, 0, false, fmt.Errorf("corrupt event at offset %d: %w", offset, decodeErr)
		}
		if ev.Seq != int64(len(events)+1) || ev.Type == "" || !json.Valid(ev.Data) || !validID.MatchString(ev.ID) || ev.Time.IsZero() {
			return nil, 0, false, fmt.Errorf("invalid event at offset %d: sequence or required fields", offset)
		}
		events = append(events, ev)
		offset += int64(len(line))
		if errors.Is(err, io.EOF) {
			return events, offset, true, nil
		}
	}
}

func (s *Store) events(id string) ([]core.Event, error) {
	if _, err := s.get(id); err != nil {
		return nil, err
	}
	dir, err := s.dir(id)
	if err != nil {
		return nil, err
	}
	events, _, _, err := readEvents(filepath.Join(dir, "events.jsonl"))
	if events == nil && err == nil {
		events = []core.Event{}
	}
	return events, err
}

func (s *Store) Events(id string) (events []core.Event, err error) {
	err = s.withLock(func() error { events, err = s.events(id); return err })
	return
}

func (s *Store) append(id, turnID, eventType string, data any) (core.Event, error) {
	session, err := s.get(id)
	if err != nil {
		return core.Event{}, err
	}
	if strings.TrimSpace(eventType) == "" {
		return core.Event{}, errors.New("event type must not be empty")
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return core.Event{}, err
	}
	dir, _ := s.dir(id)
	path := filepath.Join(dir, "events.jsonl")
	events, boundary, newline, err := readEvents(path)
	if err != nil {
		return core.Event{}, err
	}
	eventID, err := newID()
	if err != nil {
		return core.Event{}, err
	}
	ev := core.Event{Seq: int64(len(events) + 1), ID: eventID, Time: time.Now().UTC(), Type: eventType, TurnID: turnID, Data: payload}
	line, err := json.Marshal(ev)
	if err != nil {
		return core.Event{}, err
	}
	if len(line)+1 > maxEventBytes {
		return core.Event{}, fmt.Errorf("event exceeds %d bytes", maxEventBytes)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return core.Event{}, err
	}
	defer f.Close()
	// Repair only the uncommitted final fragment. Complete records are retained.
	if err := f.Truncate(boundary); err != nil {
		return core.Event{}, err
	}
	if _, err := f.Seek(boundary, io.SeekStart); err != nil {
		return core.Event{}, err
	}
	if newline {
		line = append([]byte{'\n'}, line...)
	}
	line = append(line, '\n')
	if _, err := f.Write(line); err != nil {
		return core.Event{}, err
	}
	if err := f.Sync(); err != nil {
		return core.Event{}, err
	}
	session.UpdatedAt = ev.Time
	if err := s.update(session); err != nil {
		return ev, fmt.Errorf("event committed but session metadata update failed: %w", err)
	}
	return ev, nil
}

func (s *Store) Append(id, turnID, eventType string, data any) (ev core.Event, err error) {
	err = s.withLock(func() error { ev, err = s.append(id, turnID, eventType, data); return err })
	return
}

func project(events []core.Event) ([]core.Message, error) {
	nodes, err := core.ContextSurface(events)
	if err != nil {
		return nil, err
	}
	return core.ContextMessages(nodes), nil
}

func (s *Store) Messages(id string) ([]core.Message, error) {
	events, err := s.Events(id)
	if err != nil {
		return nil, err
	}
	return project(events)
}

func (s *Store) Fork(id string, throughSeq int64) (child core.Session, err error) {
	return s.fork(id, throughSeq, false)
}

// ForkCompleted is the delegated-fork seed: only the prefix ending at the last
// settled turn is inherited. Unlike Fork(id, 0), an empty prefix means empty.
func (s *Store) ForkCompleted(id string) (child core.Session, err error) {
	return s.fork(id, 0, true)
}

func (s *Store) fork(id string, throughSeq int64, completedOnly bool) (child core.Session, err error) {
	err = s.withLock(func() error {
		parent, err := s.get(id)
		if err != nil {
			return err
		}
		events, err := s.events(id)
		if err != nil {
			return err
		}
		if throughSeq < 0 || throughSeq > int64(len(events)) {
			return errors.New("fork sequence is out of range")
		}
		if completedOnly {
			last := 0
			for i, event := range events {
				if event.Type == "turn/finished" {
					last = i + 1
				}
			}
			events = events[:last]
		} else if throughSeq > 0 {
			events = events[:throughSeq]
		}
		plan, err := forkPlanState(events)
		if err != nil {
			return err
		}
		child, err = s.create(parent.CWD, parent.Title+" (fork)")
		if err != nil {
			return err
		}
		child.ParentID = id
		child.Provider = parent.Provider
		child.Delegated = parent.Delegated
		child.Plan = plan
		child.Model = parent.Model
		child.Thinking = parent.Thinking
		child.Permission = parent.Permission
		dir, _ := s.dir(child.ID)
		cleanup := true
		defer func() {
			if cleanup {
				_ = os.RemoveAll(dir)
			}
		}()
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		for _, ev := range events {
			if err := enc.Encode(ev); err != nil {
				return err
			}
		}
		if err := atomicWrite(filepath.Join(dir, "events.jsonl"), b.Bytes()); err != nil {
			return err
		}
		if err := s.update(child); err != nil {
			return err
		}
		if _, err := s.append(child.ID, "", "session/forked", map[string]any{"parent_id": id, "through_seq": len(events)}); err != nil {
			return err
		}
		cleanup = false
		return nil
	})
	return
}

func (s *Store) Delete(id string) error {
	return s.withLock(func() error {
		if _, err := s.get(id); err != nil {
			return err
		}
		dir, _ := s.dir(id)
		f, err := openLock(filepath.Join(dir, ".turn.lock"))
		if err != nil {
			return err
		}
		defer f.Close()
		if err := lockFile(context.Background(), f, false); err != nil {
			return err
		}
		defer unlockFile(f)
		return os.RemoveAll(dir)
	})
}

func (s *Store) Export(id, format string) ([]byte, error) {
	var session core.Session
	var events []core.Event
	err := s.withLock(func() error {
		var err error
		session, err = s.get(id)
		if err != nil {
			return err
		}
		events, err = s.events(id)
		return err
	})
	if err != nil {
		return nil, err
	}
	switch format {
	case "json", "":
		return json.MarshalIndent(struct {
			Format  string       `json:"format"`
			Session core.Session `json:"session"`
			Events  []core.Event `json:"events"`
		}{"dsgo.session.v1", session, events}, "", "  ")
	case "jsonl":
		var out bytes.Buffer
		enc := json.NewEncoder(&out)
		for _, e := range events {
			if err := enc.Encode(e); err != nil {
				return nil, err
			}
		}
		return out.Bytes(), nil
	case "markdown", "md":
		var out strings.Builder
		fmt.Fprintf(&out, "# %s\n\nSession: `%s`\n\n", strings.ReplaceAll(session.Title, "\n", " "), session.ID)
		for _, ev := range events {
			if ev.Type != "user/message" && ev.Type != "assistant/message" && ev.Type != "tool/result" {
				continue
			}
			var m core.Message
			if err := json.Unmarshal(ev.Data, &m); err != nil {
				return nil, err
			}
			fmt.Fprintf(&out, "## %s\n\n", m.Role)
			for _, block := range m.Content {
				switch block.Type {
				case "text":
					out.WriteString(block.Text + "\n\n")
				case "thinking":
					out.WriteString("<details><summary>Thinking</summary>\n\n" + block.Thinking + "\n\n</details>\n\n")
				case "tool_use":
					fmt.Fprintf(&out, "Tool: `%s`\n\n```json\n%s\n```\n\n", block.Name, block.Input)
				case "tool_result":
					fmt.Fprintf(&out, "Tool result `%s` (error=%t):\n\n%v\n\n", block.ToolUseID, block.IsError, block.Content)
				default:
					fmt.Fprintf(&out, "[%s attachment]\n\n", block.Type)
				}
			}
		}
		return []byte(out.String()), nil
	default:
		return nil, errors.New("export format must be json, jsonl, or markdown")
	}
}
