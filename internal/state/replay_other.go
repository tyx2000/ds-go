//go:build !(darwin || linux || freebsd || openbsd || netbsd || dragonfly)

package state

import "os"

func openReplayKey(root *os.Root) (*os.File, error) { return root.Open(replayKeyName) }
func syncReplayDirectory(f *os.File) error          { return nil }
