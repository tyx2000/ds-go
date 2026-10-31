package tools

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"dsgo/internal/core"
)

// Fresh shell facts follow fixed shell-env.collect(). These are trusted facts
// about this execution, never values inferred from the command's arguments or
// copied from an ambient DSH_* namespace.
func freshShellEnvironment(base []string, tc core.ToolContext) ([]string, error) {
	home, err := managedHarnessHome(tc.HarnessHome)
	if err != nil {
		return nil, err
	}
	facts := map[string]string{"DSH_HOME": home, "DSH_SHELL": "1"}
	if tc.SessionID != "" {
		facts["DSH_SESSION_ID"] = tc.SessionID
	}
	if tc.ProfileName != "" || tc.ProfileDir != "" {
		facts["DSH_PROFILE"], facts["DSH_PROFILE_DIR"] = tc.ProfileName, tc.ProfileDir
	}
	for key, value := range facts {
		if strings.ContainsRune(value, '\x00') {
			return nil, errors.New("managed shell environment contains an invalid " + key)
		}
	}
	return overlayEnvironment(base, facts), nil
}

func managedHarnessHome(explicit string) (string, error) {
	selected := explicit
	if selected == "" {
		if ambient, ok := os.LookupEnv("DSH_HOME"); ok && trimEnvironmentSpace(ambient) != "" {
			selected = ambient
		}
	}
	if selected == "" || selected == "~" || strings.HasPrefix(selected, "~/") || strings.HasPrefix(selected, "~\\") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		switch {
		case selected == "":
			selected = filepath.Join(home, ".dsh")
		case selected == "~":
			selected = home
		default:
			selected = filepath.Join(home, selected[2:])
		}
	}
	if strings.ContainsRune(selected, '\x00') {
		return "", errors.New("managed shell home contains NUL")
	}
	return filepath.Abs(selected)
}

// JavaScript String.trim(), used by the official home/proxy resolvers. NEL is
// deliberately excluded, unlike strings.TrimSpace.
func trimEnvironmentSpace(value string) string {
	return strings.TrimFunc(value, func(r rune) bool {
		return r >= 0x09 && r <= 0x0d || r == 0x20 || r == 0xa0 || r == 0x1680 || r >= 0x2000 && r <= 0x200a || r == 0x2028 || r == 0x2029 || r == 0x202f || r == 0x205f || r == 0x3000 || r == 0xfeff
	})
}

// Merge one explicit, trusted layer after the credential scrub. Windows keys
// compare ignoring case; POSIX keys retain their literal spelling.
func overlayEnvironment(base []string, values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := append([]string(nil), base...)
	for _, key := range keys {
		next := out[:0]
		for _, entry := range out {
			name, _, _ := strings.Cut(entry, "=")
			match := name == key
			if runtime.GOOS == "windows" {
				match = strings.EqualFold(name, key)
			}
			if !match {
				next = append(next, entry)
			}
		}
		out = append(next, key+"="+values[key])
	}
	return out
}
