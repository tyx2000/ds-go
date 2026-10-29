package timezone

import (
	"encoding/json"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

func TestIntlAliasesAndOffsets(t *testing.T) {
	for input, want := range map[string]string{"GMT": "UTC", "utc": "UTC", "CET": "Europe/Brussels", "EST": "America/Panama", "Asia/Calcutta": "Asia/Calcutta", "+08": "+08:00", "+0830": "+08:30", "−08:30": "-08:30", "-00": "+00:00"} {
		got, location, err := Resolve(input)
		if err != nil || got != want || location == nil {
			t.Fatalf("%q => %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"", " GMT", "Z", "+24:00", "+01:60", "Unknown/Zone"} {
		if _, _, err := Resolve(input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
}

func TestIntlCatalogMatchesRuntime(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable")
	}
	names := make([]string, 0, len(zones))
	for name := range zones {
		names = append(names, name)
	}
	sort.Strings(names)
	raw, _ := json.Marshal(names)
	cmd := exec.Command(node, "--input-type=module", "--eval", `import{readFileSync}from'node:fs';let a=JSON.parse(readFileSync(0,'utf8'));process.stdout.write(JSON.stringify(a.map(timeZone=>new Intl.DateTimeFormat('en-US',{timeZone}).resolvedOptions().timeZone)));`)
	cmd.Stdin = strings.NewReader(string(raw))
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var expected []string
	if err := json.Unmarshal(output, &expected); err != nil {
		t.Fatal(err)
	}
	for i, name := range names {
		if zones[name] != expected[i] {
			t.Fatalf("%s canonical %s != Intl %s", name, zones[name], expected[i])
		}
	}
}
