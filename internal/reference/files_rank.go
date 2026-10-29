package reference

import (
	"sort"
	"strings"
	"unicode/utf16"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

func compareFileText(left, right string) int {
	// UTF-8 and UTF-16 order differ only across supplementary/BMP boundaries.
	// Keep the common ASCII and CJK filesystem ordering allocation-free.
	supplementary := func(value string) bool {
		for _, r := range value {
			if r > 0xffff {
				return true
			}
		}
		return false
	}
	if !supplementary(left) && !supplementary(right) {
		return strings.Compare(left, right)
	}
	a, b := utf16.Encode([]rune(left)), utf16.Encode([]rune(right))
	for n := 0; n < min(len(a), len(b)); n++ {
		if a[n] < b[n] {
			return -1
		}
		if a[n] > b[n] {
			return 1
		}
	}
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return 0
}
func fileTextLength(value string) int {
	length := 0
	for _, r := range value {
		length++
		if r > 0xffff {
			length++
		}
	}
	return length
}

func rankFiles(candidates []FileCandidate, query string, limit int) []FileCandidate {
	type ranked struct {
		candidate FileCandidate
		score     int
		length    int
	}
	rankedPaths := make([]ranked, 0, len(candidates))
	lower := cases.Lower(language.Und)
	needle := lower.String(query)
	for _, candidate := range candidates {
		if score, ok := scoreFile(candidate, lower.String(candidate.Path), needle); ok {
			rankedPaths = append(rankedPaths, ranked{candidate: candidate, score: score, length: fileTextLength(candidate.Path)})
		}
	}
	sort.Slice(rankedPaths, func(i, j int) bool {
		a, b := rankedPaths[i], rankedPaths[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if a.candidate.Kind != b.candidate.Kind {
			return a.candidate.Kind == "directory"
		}
		if query != "" {
			al, bl := a.length, b.length
			if al != bl {
				return al < bl
			}
		}
		return compareFileText(a.candidate.Path, b.candidate.Path) < 0
	})
	result := make([]FileCandidate, 0, min(limit, len(rankedPaths)))
	for _, entry := range rankedPaths[:min(limit, len(rankedPaths))] {
		result = append(result, entry.candidate)
	}
	return result
}

func scoreFile(candidate FileCandidate, path, query string) (int, bool) {
	if query == "" {
		return 0, true
	}
	name := path[strings.LastIndex(path, "/")+1:]
	bonus := 0
	if candidate.Kind == "directory" {
		bonus = 25
	}
	if name == query {
		return 1000 + bonus, true
	}
	if strings.HasPrefix(name, query) {
		return 900 + bonus, true
	}
	if strings.Contains(name, query) {
		return 700 + bonus, true
	}
	if strings.Contains(path, query) {
		return 500 + bonus, true
	}
	target := utf16.Encode([]rune(path))
	position, gap := 0, 0
	for _, character := range query {
		units := utf16.Encode([]rune{character})
		found := -1
		for n := position; n+len(units) <= len(target); n++ {
			same := true
			for u := range units {
				if target[n+u] != units[u] {
					same = false
					break
				}
			}
			if same {
				found = n
				break
			}
		}
		if found < 0 {
			return 0, false
		}
		gap += found - position
		position = found + 1
	}
	return 300 + max(0, 100-gap) + bonus, true
}
