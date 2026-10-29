package state

import (
	"crypto/rand"
	"errors"
	"io"
	"os"
)

const replayKeyName = ".replay-key"

// ReplayKey keys opaque model-state identities. It is independent of config
// and session exports, so an identity cannot serve as a credential hash oracle.
func (s *Store) ReplayKey() ([]byte, error) {
	var key []byte
	err := s.withLock(func() error {
		root, err := os.OpenRoot(s.root)
		if err != nil {
			return err
		}
		defer root.Close()
		key, err = readReplayKey(root)
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return err
		}
		id, err := newID()
		if err != nil {
			return err
		}
		temp := ".replay-key-" + id
		f, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer root.Remove(temp)
		_, writeErr := f.Write(key)
		if writeErr == nil {
			writeErr = f.Sync()
		}
		closeErr := f.Close()
		if err = errors.Join(writeErr, closeErr); err != nil {
			return err
		}
		// Link publishes all bytes atomically without replacing an existing key.
		if err = root.Link(temp, replayKeyName); err != nil {
			if errors.Is(err, os.ErrExist) {
				key, err = readReplayKey(root)
			}
			return err
		}
		dir, err := root.Open(".")
		if err != nil {
			return err
		}
		defer dir.Close()
		return syncReplayDirectory(dir)
	})
	if err != nil {
		return nil, err
	}
	return key, nil
}

func readReplayKey(root *os.Root) ([]byte, error) {
	info, err := root.Lstat(replayKeyName)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != 32 {
		return nil, errors.New("invalid model replay key file")
	}
	f, err := openReplayKey(root)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	current, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !current.Mode().IsRegular() || current.Size() != 32 || !os.SameFile(info, current) {
		return nil, errors.New("model replay key changed while opening")
	}
	if current.Mode().Perm()&0077 != 0 {
		return nil, errors.New("model replay key permissions must be 0600")
	}
	key, err := io.ReadAll(io.LimitReader(f, 33))
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("invalid model replay key length")
	}
	return key, nil
}
