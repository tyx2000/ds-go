package cli

import (
	"dsgo/internal/state"
	"dsgo/internal/tools"
)

func userTerminalOptions(s *state.UserTerminalSettings) tools.TerminalControllerOptions {
	var o tools.TerminalControllerOptions
	if s == nil {
		return o
	}
	if s.Shell != nil {
		o.Shell = &tools.TerminalShell{Path: s.Shell.Path, Name: s.Shell.Name, Args: append([]string{}, s.Shell.Args...)}
	}
	if s.ShellCandidates != nil {
		o.ShellCandidates = append([]string{}, (*s.ShellCandidates)...)
	}
	if s.MaxTerminals != nil {
		o.MaxTerminals = *s.MaxTerminals
	}
	if s.MaxCols != nil {
		o.MaxCols = *s.MaxCols
	}
	if s.MaxRows != nil {
		o.MaxRows = *s.MaxRows
	}
	if s.MaxBufferedBytes != nil {
		o.MaxBufferedBytes = *s.MaxBufferedBytes
	}
	if s.MaxInputBytes != nil {
		o.MaxInputBytes = *s.MaxInputBytes
	}
	if s.DisposeGraceMS != nil {
		o.DisposeGraceMS = *s.DisposeGraceMS
	}
	if s.ActivityPollIntervalMS != nil {
		o.ActivityPollIntervalMS = *s.ActivityPollIntervalMS
	}
	if s.CleanupRetryMS != nil {
		o.CleanupRetryMS = *s.CleanupRetryMS
	}
	o.Scrollback = s.Scrollback
	o.UnattendedTimeoutMS = s.UnattendedTimeoutMS
	return o
}
