package core

import (
	"strings"
	"unicode/utf16"
)

// ContextSummaryMaxChars is the pinned llm/message.ts notice-summary limit.
// Its JavaScript string length counts UTF-16 code units, not runes or bytes.
const ContextSummaryMaxChars = 120

// BoundContextSummary implements the official 120-unit limit: a longer
// summary retains its first 119 UTF-16 units and adds an ellipsis.
//
// JavaScript slice can split a surrogate pair. Go cannot store that isolated
// surrogate in valid UTF-8, so utf16.Decode replaces it with U+FFFD. This keeps
// the same limit and the replacement visible in a terminal; the boundary case
// is intentionally not byte-identical to JSON.stringify's escaped surrogate.
func BoundContextSummary(summary string) string {
	summary = strings.ToValidUTF8(summary, "\uFFFD")
	units := utf16.Encode([]rune(summary))
	if len(units) <= ContextSummaryMaxChars {
		return summary
	}
	return string(utf16.Decode(units[:ContextSummaryMaxChars-1])) + "…"
}
