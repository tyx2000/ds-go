package mcp

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// SSE accepts CR, LF and CRLF, including delimiters split across network reads.
// Unlike bufio.Scanner this imposes no unrelated per-line frame ceiling.
func readSSELines(input io.Reader, emit func([]byte) error) error {
	r := bufio.NewReaderSize(input, 64<<10)
	var line []byte
	first, skipLF := true, false
	for {
		b, err := r.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if skipLF && b == '\n' {
			skipLF = false
			continue
		}
		skipLF = false
		if b != '\r' && b != '\n' {
			line = append(line, b)
			continue
		}
		if b == '\r' {
			// The fixed parser defers a trailing CR until another character
			// establishes whether it is CRLF; EOF has no consume/flush hook.
			if _, err := r.Peek(1); err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
		}
		if first {
			line = bytes.TrimPrefix(line, []byte{0xef, 0xbb, 0xbf})
			first = false
		}
		if err := emit(line); err != nil {
			return err
		}
		line = line[:0]
		skipLF = b == '\r'
	}
}
