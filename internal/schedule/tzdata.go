package schedule

// The terminal binary must retain IANA rules on hosts without system zoneinfo.
// time.LoadLocation still prefers explicitly configured and host-supplied data.
import _ "time/tzdata"
