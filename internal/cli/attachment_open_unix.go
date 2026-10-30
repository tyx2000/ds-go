//go:build unix

package cli

import (
	"os"
	"syscall"
)

func openAttachmentFile(path string) (*os.File, error) {
	// Validate the descriptor after a nonblocking open: a pathname preflight
	// alone can race a FIFO replacement and leave the entire composer blocked.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if err := validateAttachmentFile(file); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}
