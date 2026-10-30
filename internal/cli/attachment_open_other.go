//go:build !unix

package cli

import (
	"errors"
	"os"
)

func openAttachmentFile(path string) (*os.File, error) {
	// Non-Unix hosts have no Unix FIFO open mode; reject special paths before
	// opening and revalidate the actual descriptor before reading any data.
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("attachment must be a regular file of at most 20 MiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if err := validateAttachmentFile(file); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}
