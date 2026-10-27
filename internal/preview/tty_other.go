//go:build !darwin && !linux

package preview

import "os"

func isTTY(*os.File) bool { return false }
