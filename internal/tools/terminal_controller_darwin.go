//go:build darwin

package tools

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

func terminalProcessSnapshot(ctx context.Context) (map[int]terminalProcessIdentity, error) {
	data, err := exec.CommandContext(ctx, "/bin/ps", "-axo", "pid=,ppid=,stat=,lstart=").Output()
	if err != nil {
		return nil, err
	}
	rows := map[int]terminalProcessIdentity{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 8 {
			return nil, errors.New("incomplete process identity observation")
		}
		pid, pidErr := strconv.Atoi(fields[0])
		parent, parentErr := strconv.Atoi(fields[1])
		if pidErr != nil || parentErr != nil || pid < 1 {
			return nil, errors.New("invalid process identity observation")
		}
		rows[pid] = terminalProcessIdentity{pid: pid, parent: parent, state: fields[2], started: strings.Join(fields[3:], " ")}
	}
	return rows, nil
}
