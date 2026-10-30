package cli

import (
	"context"
	"errors"
	"io"
	"os"

	"dsgo/internal/attachments"
)

func validateAttachmentFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > attachments.MaxImageBytes {
		return errors.New("attachment must be a regular file of at most 20 MiB")
	}
	return nil
}

func readAttachmentFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := openAttachmentFile(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(imageContextReader{ctx: ctx, reader: file}, attachments.MaxImageBytes+1))
	if err != nil {
		return nil, err
	}
	// A regular file may grow after the descriptor's admission stat.
	if len(data) > attachments.MaxImageBytes {
		return nil, errors.New("attachment must be a regular file of at most 20 MiB")
	}
	return data, nil
}
