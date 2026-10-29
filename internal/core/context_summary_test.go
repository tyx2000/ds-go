package core

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

func TestBoundContextSummaryUTF16Golden(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{"ascii-exact", strings.Repeat("a", 120), strings.Repeat("a", 120)},
		{"ascii-over", strings.Repeat("a", 121), strings.Repeat("a", 119) + "…"},
		{"cjk-exact", strings.Repeat("中", 120), strings.Repeat("中", 120)},
		{"cjk-over", strings.Repeat("中", 121), strings.Repeat("中", 119) + "…"},
		{"astral-exact", strings.Repeat("😀", 60), strings.Repeat("😀", 60)},
		{"astral-split", strings.Repeat("😀", 61), strings.Repeat("😀", 59) + "\uFFFD…"},
		{"astral-split-after-ascii", strings.Repeat("a", 118) + "😀x", strings.Repeat("a", 118) + "\uFFFD…"},
		{"astral-complete-at-boundary", strings.Repeat("a", 117) + "😀xx", strings.Repeat("a", 117) + "😀…"},
		{"astral-followed-by-ascii", strings.Repeat("😀", 59) + "x😀", strings.Repeat("😀", 59) + "x…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := BoundContextSummary(tc.input)
			if got != tc.want || !utf8.ValidString(got) || len(utf16.Encode([]rune(got))) > 120 {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBoundContextSummarySurrogateJSONIsValidUTF8(t *testing.T) {
	got := BoundContextSummary(strings.Repeat("a", 118) + "😀x")
	raw, err := json.Marshal(got)
	if err != nil || !json.Valid(raw) || strings.Contains(string(raw), `\ud83d`) {
		t.Fatalf("Go projection must encode a replacement, not an isolated surrogate: %s %v", raw, err)
	}
	var restored string
	if err := json.Unmarshal(raw, &restored); err != nil || restored != got || !strings.HasSuffix(restored, "\uFFFD…") {
		t.Fatalf("replacement did not survive JSON: %q %v", restored, err)
	}
	if got := BoundContextSummary("invalid\xff\xfe"); !utf8.ValidString(got) || got != "invalid\uFFFD" {
		t.Fatalf("invalid input reached terminal projection: %q", got)
	}
}
