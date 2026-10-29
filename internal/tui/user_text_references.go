package tui

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// SentUserCommand records a caller-confirmed command occurrence. Offsets use
// original UTF-8 bytes, with End exclusive; Name omits the leading slash.
type SentUserCommand struct {
	Start int    `json:"start"`
	End   int    `json:"end"`
	Name  string `json:"name"`
}

// SentUserTextFacts are semantic facts supplied by the message owner. An absent
// catalog confirms no bare session label, skill name or executed command.
// They contain no terminal visibility or interaction state.
type SentUserTextFacts struct {
	SessionLabels []string          `json:"sessionLabels,omitempty"`
	SkillNames    []string          `json:"skillNames,omitempty"`
	Commands      []SentUserCommand `json:"commands,omitempty"`
}

// SentUserSecretSpans applies the pinned sent-user reference/secret selection.
// The optional known Session labels mirror UserTextFacts.sessionLabels; unknown
// @labels are never inferred. All resulting offsets are original UTF-8 bytes.
func SentUserSecretSpans(text string, sessionLabels ...string) []SecretSpan {
	return SentUserSecretSpansWithFacts(text, SentUserTextFacts{SessionLabels: sessionLabels})
}

// MaskSentUserText changes only the text's display copy. Original history,
// model input, copy and export must keep the source string.
func MaskSentUserText(text string, sessionLabels ...string) string {
	return maskSentUserSpans(text, SentUserSecretSpans(text, sessionLabels...))
}

// MaskSentUserTextWithFacts also accepts caller-confirmed skills and commands.
func MaskSentUserTextWithFacts(text string, facts SentUserTextFacts) string {
	return maskSentUserSpans(text, SentUserSecretSpansWithFacts(text, facts))
}

// SentUserSecretSpansWithFacts mirrors UserTextDocument's nearest-start cursor.
// Secret order 0 wins same-start ties; Session order 10 precedes confirmed
// command/skill order 20 and file/folder order 30. After a reference consumes a
// cached secret, query the original text again rather than clipping that span:
// the credential scanner's addressBefore look-behind must remain available.
func SentUserSecretSpansWithFacts(text string, facts SentUserTextFacts) []SecretSpan {
	scanner := newSecretScanner(text)
	var out []SecretSpan
	var secret SecretSpan
	var reference sentUserReference
	secretQueried, secretFound, referenceQueried, referenceFound := false, false, false, false
	for cursor := 0; cursor < len(text); {
		if !secretQueried || secretFound && secret.Start < cursor {
			span, ok := scanner.match(sort.SearchInts(scanner.bytes, cursor))
			secretFound, secretQueried = ok, true
			if ok {
				secret = scanner.byteSpan(span)
			}
		}
		if !referenceQueried || referenceFound && reference.start < cursor {
			reference, referenceFound = nextSentUserReference(text, cursor, facts)
			referenceQueried = true
		}
		if !secretFound && !referenceFound {
			break
		}
		if referenceFound && (!secretFound || reference.start < secret.Start) {
			cursor = reference.end
			continue
		}
		out = append(out, secret)
		cursor = secret.End
	}
	return out
}

func maskSentUserSpans(text string, spans []SecretSpan) string {
	if len(spans) == 0 {
		return text
	}
	var out strings.Builder
	from := 0
	for _, span := range spans {
		out.WriteString(text[from:span.Start])
		out.WriteString(span.Masked)
		from = span.End
	}
	out.WriteString(text[from:])
	return out.String()
}

type sentUserReference struct {
	start, end, order int
	id                string
}

func sentUserSpace(r rune) bool {
	return (r >= 9 && r <= 13) || r == ' ' || secretWideSpace(int(r))
}

func sentUserBoundaryBefore(text string, at int) bool {
	if at == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(text[:at])
	return sentUserSpace(r)
}

func sentUserBoundaryAfter(text string, at int) bool {
	if at == len(text) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(text[at:])
	return sentUserSpace(r)
}

func nextSentUserReference(text string, from int, facts SentUserTextFacts) (sentUserReference, bool) {
	best, found := nextSentSessionReference(text, from, facts.SessionLabels)
	accept := func(candidate sentUserReference) {
		if !found || candidate.start < best.start || candidate.start == best.start && (candidate.order < best.order || candidate.order == best.order && candidate.id < best.id) {
			best, found = candidate, true
		}
	}
	for _, command := range facts.Commands {
		if command.Start < from || command.End > len(text) || command.End < command.Start || text[command.Start:command.End] != "/"+command.Name || !sentUserBoundaryBefore(text, command.Start) || !sentUserBoundaryAfter(text, command.End) {
			continue
		}
		candidate := sentUserReference{command.Start, command.End, 20, "command"}
		if found && best.id == "command" && best.start == candidate.start && candidate.end > best.end {
			best = candidate
		} else {
			accept(candidate)
		}
	}
	if candidate, ok := nextSentSkillReference(text, from, facts.SkillNames); ok {
		accept(candidate)
	}
	if candidate, ok := nextSentPathReference(text, from); ok {
		accept(candidate)
	}
	return best, found
}

func nextSentSessionReference(text string, from int, labels []string) (sentUserReference, bool) {
	for cursor := from; cursor < len(text); {
		relative := strings.IndexByte(text[cursor:], '@')
		if relative < 0 {
			break
		}
		start := cursor + relative
		if end, ok := sentSessionWireEnd(text, start); ok {
			return sentUserReference{start, end, 10, "session"}, true
		}
		end := -1
		for _, label := range labels {
			if label != "" && strings.HasPrefix(text[start+1:], label) {
				end = max(end, start+1+len(label))
			}
		}
		if end >= 0 {
			return sentUserReference{start, end, 10, "session"}, true
		}
		cursor = start + 1
	}
	return sentUserReference{}, false
}

// The UI's wire grammar deliberately does not canonicalize the URI: it is
// @[nonempty label without ']' or LF](dsh-session:nonempty nonspace payload).
func sentSessionWireEnd(text string, start int) (int, bool) {
	if !strings.HasPrefix(text[start:], "@[") {
		return 0, false
	}
	label := start + 2
	close := label
	for close < len(text) && text[close] != ']' && text[close] != '\n' {
		close++
	}
	if close == label || !strings.HasPrefix(text[close:], "](dsh-session:") {
		return 0, false
	}
	payload := close + len("](dsh-session:")
	end := payload
	for end < len(text) {
		r, size := utf8.DecodeRuneInString(text[end:])
		if r == ')' || sentUserSpace(r) {
			break
		}
		end += size
	}
	return end + 1, end > payload && end < len(text) && text[end] == ')'
}

func nextSentSkillReference(text string, from int, names []string) (sentUserReference, bool) {
	if len(names) == 0 {
		return sentUserReference{}, false
	}
	for cursor := from; cursor < len(text); {
		relative := strings.IndexByte(text[cursor:], '/')
		if relative < 0 {
			break
		}
		start := cursor + relative
		cursor = start + 1
		if !sentUserBoundaryBefore(text, start) {
			continue
		}
		end := start + 1
		for end < len(text) && (secretClass(int(text[end]))&secretWord != 0 || text[end] == '-') {
			end++
		}
		if end == start+1 || !sentUserBoundaryAfter(text, end) {
			continue
		}
		name := text[start+1 : end]
		for _, confirmed := range names {
			if confirmed == name {
				return sentUserReference{start, end, 20, "skill"}, true
			}
		}
	}
	return sentUserReference{}, false
}

// nextPath is the pinned /(?<!\S)(@"[^"\n]+"|@[^\s]+)/gu rule.
// An incomplete quote can fall through to the second alternative exactly as
// it does in the official regexp. Only unquoted tokens trim final punctuation.
func nextSentPathReference(text string, from int) (sentUserReference, bool) {
	for cursor := from; cursor < len(text); {
		relative := strings.IndexByte(text[cursor:], '@')
		if relative < 0 {
			break
		}
		start := cursor + relative
		cursor = start + 1
		if !sentUserBoundaryBefore(text, start) {
			continue
		}
		end := -1
		quoted := strings.HasPrefix(text[start:], "@\"")
		if quoted {
			for at := start + 2; at < len(text) && text[at] != '\n'; at++ {
				if text[at] == '"' {
					if at > start+2 {
						end = at + 1
					}
					break
				}
			}
		}
		if end < 0 {
			end = start + 1
			for end < len(text) {
				r, size := utf8.DecodeRuneInString(text[end:])
				if sentUserSpace(r) {
					break
				}
				end += size
			}
		}
		cursor = end
		if !quoted {
			for end > start+1 {
				r, size := utf8.DecodeLastRuneInString(text[start+1 : end])
				if !strings.ContainsRune(".,;:!?，。；：！？", r) {
					break
				}
				end -= size
			}
		}
		if end > start+1 {
			return sentUserReference{start, end, 30, "file"}, true
		}
	}
	return sentUserReference{}, false
}
