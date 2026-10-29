package tui

import (
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// SecretSpan is one display-only credential range. Start and End are UTF-8
// byte offsets into the original text, with End exclusive. Masked replaces
// hidden code points with '*', keeping the source's CR and LF line breaks.
type SecretSpan struct {
	Start  int    `json:"start"`
	End    int    `json:"end"`
	Masked string `json:"masked"`
}

// SecretSpans finds the published credential formats recognized by the pinned
// ui-secret-mask rule. It does not recognize arbitrary passwords or assignments.
func SecretSpans(text string) []SecretSpan {
	scanner := newSecretScanner(text)
	var spans []SecretSpan
	for from := 0; from < len(scanner.text); {
		span, ok := scanner.match(from)
		if !ok {
			break
		}
		spans = append(spans, scanner.byteSpan(span))
		from = span.end
	}
	return spans
}

// FindSecretSpan finds the nearest credential beginning at or after from,
// measured in UTF-8 bytes. An offset inside a UTF-8 encoding advances to its
// next code point. Unlike filtering SecretSpans, this preserves the official
// address-password look-behind when a preceding reference consumed its scheme.
func FindSecretSpan(text string, from int) (SecretSpan, bool) {
	if from < 0 {
		from = 0
	}
	if from > len(text) {
		return SecretSpan{}, false
	}
	for from < len(text) && !utf8.RuneStart(text[from]) {
		from++
	}
	scanner := newSecretScanner(text)
	unit := sort.SearchInts(scanner.bytes, from)
	span, ok := scanner.match(unit)
	if !ok {
		return SecretSpan{}, false
	}
	return scanner.byteSpan(span), true
}

// MaskUserText renders sent user text without changing its original value.
// Callers must retain the original for history, model input, export and copy.
func MaskUserText(text string) string {
	spans := SecretSpans(text)
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

type secretRange struct {
	start, end int
	kind       int
	plain      bool
}

type secretScanner struct {
	text  []uint16
	bytes []int // UTF-16 offset -> original UTF-8 byte offset
	memo  [27]int
}

func newSecretScanner(text string) *secretScanner {
	s := &secretScanner{}
	for offset, r := range text {
		if r <= 0xffff {
			s.text = append(s.text, uint16(r))
			s.bytes = append(s.bytes, offset)
		} else {
			high, low := utf16.EncodeRune(r)
			s.text = append(s.text, uint16(high), uint16(low))
			s.bytes = append(s.bytes, offset, offset)
		}
	}
	s.bytes = append(s.bytes, len(text))
	for i := range s.memo {
		s.memo[i] = -1
	}
	return s
}

func (s *secretScanner) byteSpan(span secretRange) SecretSpan {
	return SecretSpan{Start: s.bytes[span.start], End: s.bytes[span.end], Masked: s.masked(span)}
}

func (s *secretScanner) code(at int) int {
	if at < 0 || at >= len(s.text) {
		return -1
	}
	return int(s.text[at])
}

func (s *secretScanner) has(at int, literal string) bool {
	if at < 0 || at+len(literal) > len(s.text) {
		return false
	}
	for i := range len(literal) {
		if s.text[at+i] != uint16(literal[i]) {
			return false
		}
	}
	return true
}

// The literal-only scan resumes at a proper suffix that can start another
// format, including a private-key header whose first '-' overlaps an sk- run.
func (s *secretScanner) nextLiteral(from int) (at, format int, ok bool) {
	for at := from; at < len(s.text); at++ {
		code := s.code(at)
		if code >= 128 {
			continue
		}
		for _, index := range secretLiteralHeads[code] {
			if s.has(at, secretFormats[index].literal) {
				return at, index, true
			}
		}
	}
	return 0, 0, false
}

// match follows CredentialScanner.match's deferred address start, earliest
// candidate mask, and transitive overlap rules. All internal offsets are UTF-16
// units so address limits and queries match JavaScript's string indexing.
func (s *secretScanner) match(from int) (secretRange, bool) {
	span := secretRange{start: -1, end: -1, kind: secretPrefixed, plain: true}
	deferredEnd := s.addressBefore(from)
	deferredStart := -1
	if deferredEnd >= 0 {
		deferredStart = s.memo[26]
	}
	for cursor := from; ; {
		at, index, ok := s.nextLiteral(cursor)
		if !ok {
			break
		}
		format := secretFormats[index]
		cursor = at + len(format.literal) - secretLiteralOverlap[index]
		if deferredStart >= 0 && deferredStart <= at {
			if span.start < 0 {
				span.start, span.end, span.kind = deferredStart, deferredEnd, secretAddress
			} else if deferredStart >= span.end {
				break
			}
			deferredStart = -1
		}
		if span.start >= 0 && (at >= span.end || span.end == len(s.text)) {
			break
		}
		if format.kind == secretAddress {
			deferredEnd = s.addressPassword(at)
			deferredStart = -1
			if deferredEnd >= 0 {
				deferredStart = s.memo[26]
			}
			continue
		}
		end := -1
		if format.kind == secretKeyBlock {
			end = s.keyBlockEnd(at)
		} else {
			end = s.prefixedEnd(format, at)
		}
		if end < 0 {
			continue
		}
		if span.start < 0 {
			span = secretRange{at, end, format.kind, format.kind == secretPrefixed}
		} else if end > span.end {
			span.end = end
			if format.kind != secretPrefixed {
				span.plain = false
			}
		}
	}
	if span.start < 0 && deferredStart >= 0 {
		span.start, span.end, span.kind = deferredStart, deferredEnd, secretAddress
	}
	return span, span.start >= 0
}

func (s *secretScanner) isWord(at int) bool {
	return secretClass(s.code(at))&secretWord != 0
}

func secretWideSpace(code int) bool {
	return code == 0xa0 || code == 0x1680 || (code >= 0x2000 && code <= 0x200a) || code == 0x2028 || code == 0x2029 || code == 0x202f || code == 0x205f || code == 0x3000 || code == 0xfeff
}

func secretUserCode(code int) bool {
	if code < 0 {
		return false
	}
	if code < 128 {
		return secretClass(code)&secretUser != 0
	}
	return !secretWideSpace(code)
}

func secretPasswordCode(code int) bool {
	if code < 0 {
		return false
	}
	if code < 128 {
		return secretClass(code)&secretPassword != 0
	}
	return !secretWideSpace(code)
}

func (s *secretScanner) runEnd(start, class int) int {
	end := start
	for end < len(s.text) {
		code := s.code(end)
		valid := secretClass(code)&class != 0
		if class == secretUser {
			valid = secretUserCode(code)
		} else if class == secretPassword {
			valid = secretPasswordCode(code)
		}
		if !valid {
			break
		}
		end++
	}
	return end
}

func (s *secretScanner) keyRunEnd(start int) int {
	for slot := 0; slot < 8; slot += 2 {
		if s.memo[slot] <= start && start <= s.memo[slot+1] {
			return s.memo[slot+1]
		}
	}
	end := s.runEnd(start, secretKey)
	next := (s.memo[8] + 1) & 3
	s.memo[8] = next
	s.memo[2*next], s.memo[2*next+1] = start, end
	return end
}

func (s *secretScanner) memoRunEnd(start, slot, class int) int {
	if s.memo[slot] <= start && start <= s.memo[slot+1] {
		return s.memo[slot+1]
	}
	end := s.runEnd(start, class)
	s.memo[slot], s.memo[slot+1] = start, end
	return end
}

func (s *secretScanner) nextDigit(start int) int {
	if s.memo[21] <= start && start <= s.memo[22] {
		return s.memo[22]
	}
	end := start
	for end < len(s.text) && (s.text[end] < '0' || s.text[end] > '9') {
		end++
	}
	s.memo[21], s.memo[22] = start, end
	return end
}

func (s *secretScanner) labelEnd(start int) int {
	index := start
	for limit := min(len(s.text), start+52); index < limit; index++ {
		if secretClass(s.code(index))&secretLabel == 0 {
			break
		}
	}
	tail := index - len("PRIVATE KEY")
	if index < start+52 && tail >= start && s.has(tail, "PRIVATE KEY-----") {
		return tail + len("PRIVATE KEY-----")
	}
	return -1
}

func (s *secretScanner) keyBlockEnd(at int) int {
	header := s.labelEnd(at + len("-----BEGIN "))
	if header < 0 {
		return -1
	}
	if s.memo[23] <= header && header <= s.memo[24] {
		return s.memo[25]
	}
	s.memo[23], s.memo[24], s.memo[25] = header, len(s.text), len(s.text)
	for line := header; line < len(s.text); line++ {
		if !s.has(line, "-----END ") {
			continue
		}
		if end := s.labelEnd(line + len("-----END ")); end >= 0 {
			s.memo[24], s.memo[25] = line, end
			break
		}
	}
	return s.memo[25]
}

func (s *secretScanner) schemeBefore(at int) bool {
	for start := at - 1; start >= 0 && start >= at-16; start-- {
		bits := secretClass(s.code(start))
		if bits&secretScheme == 0 {
			return false
		}
		if bits&secretLower != 0 && at-start >= 2 && !s.isWord(start-1) {
			return true
		}
	}
	return false
}

func (s *secretScanner) addressPassword(at int) int {
	if !s.schemeBefore(at) {
		return -1
	}
	userStart := at + 3
	userEnd := s.memoRunEnd(userStart, 17, secretUser)
	if userEnd-userStart > 256 || s.code(userEnd) != ':' {
		return -1
	}
	start := userEnd + 1
	end := s.memoRunEnd(start, 19, secretPassword)
	if end == start || s.code(end) != '@' {
		return -1
	}
	s.memo[26] = start
	return end
}

func (s *secretScanner) addressBefore(from int) int {
	if from < 3 {
		return -1
	}
	if s.has(from-1, "://") {
		return s.addressPassword(from - 1)
	}
	if s.has(from-2, "://") {
		return s.addressPassword(from - 2)
	}
	index := from - 1
	before := s.code(index)
	passwordStart := -1
	if before == ':' {
		passwordStart = from
		index--
	} else if secretUserCode(before) {
		userEnd := s.memoRunEnd(from, 17, secretUser)
		if s.code(userEnd) != ':' {
			return -1
		}
		passwordStart = userEnd + 1
	} else {
		if s.has(from-3, "://") {
			return s.addressPassword(from - 3)
		}
		return -1
	}
	passwordEnd := s.memoRunEnd(passwordStart, 19, secretPassword)
	if passwordEnd == passwordStart || s.code(passwordEnd) != '@' {
		return -1
	}
	for lowest := max(0, passwordStart-258); index >= lowest && secretUserCode(s.code(index)); index-- {
	}
	if index >= 2 && s.has(index-2, "://") {
		return s.addressPassword(index - 2)
	}
	return -1
}

func (s *secretScanner) prefixedEnd(format secretFormat, at int) int {
	if s.isWord(at - 1) {
		return -1
	}
	start := at + len(format.literal)
	if format.shape == secretSK {
		digit := s.nextDigit(start)
		if digit-start > 255 {
			return -1
		}
		end := s.keyRunEnd(start)
		if end-start >= format.min && digit < end {
			return end
		}
		return -1
	}
	end := -1
	if format.slot == 0 {
		end = s.keyRunEnd(start)
	} else {
		end = s.memoRunEnd(start, format.slot, format.run)
	}
	length := end - start
	if length < format.min {
		return -1
	}
	switch format.shape {
	case secretRun:
		return end
	case secretGoogle:
		return start + format.min
	case secretBounded:
		if length <= format.max && !s.isWord(end) {
			return end
		}
		return -1
	}
	if length > format.max || !s.has(end, ".eyJ") {
		return -1
	}
	payloadStart := end + 4
	payloadEnd := s.keyRunEnd(payloadStart)
	if payloadEnd-payloadStart < format.min || s.code(payloadEnd) != '.' {
		return -1
	}
	signatureEnd := s.keyRunEnd(payloadEnd + 1)
	if signatureEnd-(payloadEnd+1) >= format.min {
		return signatureEnd
	}
	return -1
}

func (s *secretScanner) unitSlice(start, end int) string {
	return string(utf16.Decode(s.text[start:end]))
}

func (s *secretScanner) maskRange(start, end int) string {
	var out strings.Builder
	for index := start; index < end; index++ {
		code := s.text[index]
		if code == '\n' || code == '\r' {
			out.WriteByte(byte(code))
			continue
		}
		if code >= 0xd800 && code <= 0xdbff && index+1 < end && s.text[index+1] >= 0xdc00 && s.text[index+1] <= 0xdfff {
			index++
		}
		out.WriteByte('*')
	}
	return out.String()
}

func (s *secretScanner) masked(span secretRange) string {
	if span.kind == secretAddress {
		return s.maskRange(span.start, span.end)
	}
	if span.kind == secretPrefixed {
		prefix := s.unitSlice(span.start, span.start+8)
		if span.plain {
			return prefix + strings.Repeat("*", span.end-span.start-8)
		}
		return prefix + s.maskRange(span.start+8, span.end)
	}
	bodyStart, bodyEnd := span.start, span.end
	for index := span.start; index < span.end; index++ {
		if s.text[index] == '\n' {
			bodyStart = index + 1
			break
		}
	}
	for index := span.end - len("\n-----END"); index >= bodyStart; index-- {
		if s.text[index] == '\n' && s.has(index+1, "-----END") {
			bodyEnd = index
			break
		}
	}
	return s.unitSlice(span.start, bodyStart) + s.maskRange(bodyStart, bodyEnd) + s.unitSlice(bodyEnd, span.end)
}
