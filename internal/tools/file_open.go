package tools

import (
	"errors"
	"os"
)

// Validate the opened descriptor, not only the path: a path may be replaced
// between a stat and an open. The platform opener must not wait for a FIFO peer.
func openedRegularFile(f *os.File, err error) (*os.File, os.FileInfo, error) {
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("path is not a regular file")
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

func openRegularFile(path string) (*os.File, os.FileInfo, error) {
	return openedRegularFile(openReadNoWait(path))
}

func openRegularAt(root *os.Root, path string) (*os.File, os.FileInfo, error) {
	return openedRegularFile(openRootReadNoWait(root, path))
}
