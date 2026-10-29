package state

import (
	"dsgo/internal/timezone"
	"fmt"
)

type TimeContextSettings struct {
	TimeZone          string `json:"time_zone,omitempty"`
	RefreshIntervalMS *int64 `json:"refresh_interval_ms,omitempty"`
}

func (s *TimeContextSettings) validate() error {
	if s == nil {
		return nil
	}
	if s.RefreshIntervalMS != nil && (*s.RefreshIntervalMS < 0 || *s.RefreshIntervalMS > 9007199254740991) {
		return fmt.Errorf("time_context.refresh_interval_ms must be a non-negative safe integer")
	}
	if s.TimeZone != "" {
		_, _, err := timezone.Resolve(s.TimeZone)
		return err
	}
	return nil
}
