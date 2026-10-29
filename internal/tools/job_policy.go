package tools

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
)

func (r *Registry) SetJobWait(defaultMS, maximumMS float64) error {
	if defaultMS < 1 || maximumMS < 1 || defaultMS > maximumMS || math.IsNaN(defaultMS) || math.IsNaN(maximumMS) || math.IsInf(defaultMS, 0) || math.IsInf(maximumMS, 0) {
		return errors.New("invalid job wait configuration")
	}
	r.mu.Lock()
	r.jobWaitDefault, r.jobWaitCap = defaultMS, maximumMS
	r.mu.Unlock()
	return nil
}
func (r *Registry) JobWait(timeoutMS *float64) (time.Duration, error) {
	r.mu.RLock()
	defaultMS, maximumMS := r.jobWaitDefault, r.jobWaitCap
	r.mu.RUnlock()
	if defaultMS == 0 {
		defaultMS = jobWaitMS
	}
	if maximumMS == 0 {
		maximumMS = jobMaxWaitMS
	}
	return resolveJobWait(timeoutMS, defaultMS, maximumMS)
}
func resolveJobWait(timeoutMS *float64, defaultMS, maximumMS float64) (time.Duration, error) {
	value := defaultMS
	if timeoutMS != nil {
		value = *timeoutMS
	}
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("invalid timeout_ms: expected a positive number, got %s", strconv.FormatFloat(value, 'f', -1, 64))
	}
	const maxMilliseconds = int64(1<<63-1) / int64(time.Millisecond)
	return time.Duration(min(math.Ceil(value), maximumMS, float64(maxMilliseconds))) * time.Millisecond, nil
}
