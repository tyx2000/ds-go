package core

import "regexp"

// These are the pinned llm/error.ts provider-neutral overflow classifiers.
// Classify before discarding untrusted provider text; callers retain only the
// stable error code, never the response body.
var contextOverflowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:^|[^a-z0-9])context[\s_-](?:length|window)[\s_-](?:exceed(?:ed|s)?|overflow(?:ed)?|limit[\s_-]exceeded)(?:$|[^a-z0-9])`),
	regexp.MustCompile(`(?i)\b(?:maximum|max)(?:\s+(?:allowed|supported))?\s+context\s+(?:length|window)\b`),
	regexp.MustCompile(`(?i)\b(?:request|prompt|input|messages?)\s+(?:is\s+|are\s+)?too\s+(?:large|long)\s+for\s+(?:(?:this|the)\s+)?(?:model(?:'s)?\s+)?context(?:\s+window)?\b`),
	regexp.MustCompile(`(?i)\b(?:input|prompt|request)\s+(?:is\s+)?too\s+(?:long|large)\s+for\s+(?:this|the)\s+model\b`),
	regexp.MustCompile(`(?i)\b(?:input|prompt|request|messages?)\b.{0,40}\b(?:exceed(?:s|ed)?|overflows?|is\s+larger\s+than)\b.{0,40}\b(?:the\s+)?(?:model(?:'s)?\s+)?context(?:\s+(?:length|window))?\b`),
}

func IsContextWindowExceeded(detail string) bool {
	for _, pattern := range contextOverflowPatterns {
		if pattern.MatchString(detail) {
			return true
		}
	}
	return false
}
