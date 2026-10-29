package reference

import "strings"

type AtToken struct {
	Prefix, Query string
	Quoted        bool
	Start, End    int
}

// jsSpace matches ECMAScript \s, rather than broadening it with Unicode NEL.
func jsSpace(r rune) bool {
	switch r {
	case '\t', '\v', '\f', ' ', '\u00a0', '\ufeff', '\n', '\r', '\u2028', '\u2029', '\u1680', '\u202f', '\u205f', '\u3000':
		return true
	}
	return r >= '\u2000' && r <= '\u200a'
}

func ActiveAtToken(line string, cursorRune int) (AtToken, bool) {
	text := []rune(line)
	if cursorRune < 0 || cursorRune > len(text) {
		return AtToken{}, false
	}
	text = text[:cursorRune]
	// The quoted alternative may contain whitespace; the final quote makes it
	// ineligible, exactly as the official open-token regular expression does.
	for start := len(text) - 2; start >= 0; start-- {
		if text[start] != '@' || text[start+1] != '"' || start > 0 && !jsSpace(text[start-1]) {
			continue
		}
		if !strings.ContainsRune(string(text[start+2:]), '"') {
			return AtToken{Prefix: string(text[start:]), Query: string(text[start+2:]), Quoted: true, Start: start, End: cursorRune}, true
		}
	}
	start := len(text)
	for start > 0 && !jsSpace(text[start-1]) {
		start--
	}
	if start >= len(text) || text[start] != '@' {
		return AtToken{}, false
	}
	return AtToken{Prefix: string(text[start:]), Query: string(text[start+1:]), Start: start, End: cursorRune}, true
}

func FormatFileMention(path, kind string, preserveQuote ...bool) (string, bool) {
	if kind == "directory" {
		path += "/"
	}
	quoted := len(preserveQuote) > 0 && preserveQuote[0]
	for _, r := range path {
		if r <= 0x1f || r >= 0x7f && r <= 0x9f || r == '"' {
			return "", false
		}
		if jsSpace(r) {
			quoted = true
		}
	}
	if !quoted {
		return "@" + path, true
	}
	if kind == "directory" {
		return "@\"" + path, true
	}
	return "@\"" + path + "\"", true
}
