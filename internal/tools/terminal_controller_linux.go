//go:build linux

package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func terminalProcessSnapshot(ctx context.Context) (map[int]terminalProcessIdentity, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	rows := map[int]terminalProcessIdentity{}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pid, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil || pid < 1 {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return nil, readErr
		}
		end := strings.LastIndexByte(string(data), ')')
		if end < 0 {
			return nil, errors.New("invalid proc process identity")
		}
		fields := strings.Fields(string(data[end+1:]))
		if len(fields) < 20 {
			return nil, errors.New("incomplete proc process identity")
		}
		parent, parentErr := strconv.Atoi(fields[1])
		session, sessionErr := strconv.Atoi(fields[3])
		if parentErr != nil || sessionErr != nil {
			return nil, errors.New("invalid proc process identity fields")
		}
		rows[pid] = terminalProcessIdentity{pid: pid, parent: parent, session: session, state: fields[0], started: fields[19]}
	}
	return rows, nil
}
