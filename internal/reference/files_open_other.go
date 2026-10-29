//go:build !darwin && !linux

package reference

import "os"

func openFileDirectory(root *os.Root, name string) (*os.File, error) { return root.Open(name + "/.") }
