//go:build !darwin && !linux

package tui

func watchLineResize(func()) func() { return func() {} }
