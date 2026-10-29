//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package context

import "os"

func openInstructionFile(path string) (*os.File, error) { return os.Open(path) }
func isInstructionNotDirectory(error) bool              { return false }
