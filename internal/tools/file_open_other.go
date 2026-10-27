//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package tools

import (
	"errors"
	"os"
)

// These platforms have no Unix FIFO opener. Reject known special paths before
// opening and validate the resulting descriptor in the common helper too.
func openReadNoWait(path string) (*os.File, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("path is not a regular file")
	}
	return os.Open(path)
}

func openRootReadNoWait(root *os.Root, path string) (*os.File, error) {
	info, err := root.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("path is not a regular file")
	}
	return root.Open(path)
}
