package state

import (
	"encoding/json"
	"testing"
)

func TestUserTerminalSettingsOmissionAndExplicitZero(t *testing.T) {
	for _, test := range []struct {
		raw   string
		valid bool
	}{
		{`{}`, true}, {`{"scrollback":0,"unattended_timeout_ms":0,"shell_candidates":[]}`, true},
		{`{"max_terminals":0}`, false}, {`{"max_cols":1}`, false}, {`{"max_rows":0}`, false},
		{`{"max_buffered_bytes":1023}`, false}, {`{"max_input_bytes":0}`, false},
		{`{"dispose_grace_ms":0}`, false}, {`{"activity_poll_interval_ms":0}`, false},
		{`{"cleanup_retry_ms":9007199254740992}`, false}, {`{"shell":{"path":"sh"}}`, false},
		{`{"shell_candidates":[""]}`, false},
	} {
		t.Run(test.raw, func(t *testing.T) {
			cfg := DefaultConfig()
			if err := json.Unmarshal([]byte(test.raw), &cfg.UserTerminal); err != nil {
				t.Fatal(err)
			}
			if err := cfg.Validate(); (err == nil) != test.valid {
				t.Fatalf("valid=%v: %v", test.valid, err)
			}
		})
	}
	root := t.TempDir()
	cfg := DefaultConfig()
	if err := json.Unmarshal([]byte(`{"scrollback":0,"unattended_timeout_ms":0,"shell_candidates":[]}`), &cfg.UserTerminal); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(root, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.UserTerminal == nil || got.UserTerminal.Scrollback == nil || *got.UserTerminal.Scrollback != 0 || got.UserTerminal.UnattendedTimeoutMS == nil || *got.UserTerminal.UnattendedTimeoutMS != 0 || got.UserTerminal.ShellCandidates == nil || len(*got.UserTerminal.ShellCandidates) != 0 {
		t.Fatalf("explicit zero/empty settings changed: %#v", got.UserTerminal)
	}
}
