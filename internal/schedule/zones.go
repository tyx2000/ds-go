package schedule

import (
	_ "embed"
	"encoding/json"
)

// canonicalZoneData records the canonicalization of the official Intl algorithm
// against the supported IANA name vocabulary. The generator records its ICU/TZ
// versions. Recurrence arithmetic continues to use the Host's Go IANA data.
//
//go:embed zones.json
var canonicalZoneData []byte

var canonicalZones = func() map[string]string {
	var data struct {
		Zones map[string]string `json:"zones"`
	}
	if err := json.Unmarshal(canonicalZoneData, &data); err != nil {
		panic("invalid embedded schedule zone names")
	}
	return data.Zones
}()
