// Package timezone preserves the pinned runtime's Intl zone-name aliases.
package timezone

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

//go:embed zones.json
var zoneData []byte

var zones = func() map[string]string {
	var data struct {
		Zones map[string]string `json:"zones"`
	}
	if err := json.Unmarshal(zoneData, &data); err != nil {
		panic(err)
	}
	return data.Zones
}()

var offsetPattern = regexp.MustCompile(`^([+\-−])([0-9]{2})(?::?([0-9]{2}))?$`)

// Resolve accepts Intl's named aliases and fixed-offset spellings, preserving
// its canonical label. Calendar arithmetic uses the Host Go IANA data.
func Resolve(name string) (string, *time.Location, error) {
	if fields := offsetPattern.FindStringSubmatch(name); fields != nil {
		hour, _ := strconv.Atoi(fields[2])
		minute := 0
		if fields[3] != "" {
			minute, _ = strconv.Atoi(fields[3])
		}
		if hour < 24 && minute < 60 {
			offset := hour*3600 + minute*60
			sign := "+"
			if fields[1] != "+" && offset != 0 {
				sign = "-"
				offset = -offset
			}
			canonical := fmt.Sprintf("%s%02d:%02d", sign, hour, minute)
			return canonical, time.FixedZone(canonical, offset), nil
		}
	}
	canonical, ok := zones[strings.ToLower(name)]
	if ok {
		location, err := time.LoadLocation(canonical)
		if err == nil {
			return canonical, location, nil
		}
	}
	return "", nil, fmt.Errorf("unsupported time zone %q", name)
}
