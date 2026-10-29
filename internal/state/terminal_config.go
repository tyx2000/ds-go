package state

import "fmt"

type UserTerminalShell struct {
	Path string   `json:"path"`
	Name string   `json:"name"`
	Args []string `json:"args,omitempty"`
}

// UserTerminalSettings applies to the explicit Host terminal, independently of
// model Bash, permission mode and the minimal preset's private shell.
type UserTerminalSettings struct {
	Shell                  *UserTerminalShell `json:"shell,omitempty"`
	ShellCandidates        *[]string          `json:"shell_candidates,omitempty"`
	MaxTerminals           *int               `json:"max_terminals,omitempty"`
	MaxCols                *int               `json:"max_cols,omitempty"`
	MaxRows                *int               `json:"max_rows,omitempty"`
	Scrollback             *int               `json:"scrollback,omitempty"`
	MaxBufferedBytes       *int               `json:"max_buffered_bytes,omitempty"`
	MaxInputBytes          *int               `json:"max_input_bytes,omitempty"`
	DisposeGraceMS         *int64             `json:"dispose_grace_ms,omitempty"`
	UnattendedTimeoutMS    *int64             `json:"unattended_timeout_ms,omitempty"`
	ActivityPollIntervalMS *int64             `json:"activity_poll_interval_ms,omitempty"`
	CleanupRetryMS         *int64             `json:"cleanup_retry_ms,omitempty"`
}

func (s *UserTerminalSettings) validate() error {
	if s == nil {
		return nil
	}
	for _, field := range []struct {
		name  string
		value *int
		min   int
	}{
		{"max_terminals", s.MaxTerminals, 1}, {"max_cols", s.MaxCols, 2}, {"max_rows", s.MaxRows, 1},
		{"scrollback", s.Scrollback, 0}, {"max_buffered_bytes", s.MaxBufferedBytes, 1024}, {"max_input_bytes", s.MaxInputBytes, 1},
	} {
		if field.value != nil && *field.value < field.min {
			return fmt.Errorf("user_terminal.%s must be at least %d", field.name, field.min)
		}
	}
	for _, field := range []struct {
		name  string
		value *int64
		min   int64
	}{
		{"dispose_grace_ms", s.DisposeGraceMS, 1}, {"unattended_timeout_ms", s.UnattendedTimeoutMS, 0},
		{"activity_poll_interval_ms", s.ActivityPollIntervalMS, 1}, {"cleanup_retry_ms", s.CleanupRetryMS, 1},
	} {
		if field.value != nil && (*field.value < field.min || *field.value > 9007199254740991) {
			return fmt.Errorf("user_terminal.%s must be a safe integer at least %d", field.name, field.min)
		}
	}
	if s.Shell != nil && (s.Shell.Path == "" || s.Shell.Name == "") {
		return fmt.Errorf("user_terminal.shell requires path and name")
	}
	if s.ShellCandidates != nil {
		for _, path := range *s.ShellCandidates {
			if path == "" {
				return fmt.Errorf("user_terminal.shell_candidates must contain nonempty strings")
			}
		}
	}
	return nil
}
