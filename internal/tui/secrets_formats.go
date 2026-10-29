package tui

// Pinned ui-secret-mask credential-formats.ts/format-tables.ts vocabulary.
const (
	secretKey = 1 << iota
	secretAlnum
	secretWord
	secretAlnumDash
	secretUpperAlnum
	secretLabel
	secretScheme
	secretLower
	secretUser
	secretPassword
)

const (
	secretKeyBlock = iota
	secretAddress
	secretPrefixed
)

const (
	secretRun = iota
	secretBounded
	secretSK
	secretGoogle
	secretJWT
)

type secretFormat struct {
	literal                    string
	kind, shape, run, min, max int
	slot                       int
}

func prefixedSecret(literal string, shape, run, min, max int) secretFormat {
	slot := 0
	switch run {
	case secretAlnum:
		slot = 9
	case secretWord:
		slot = 11
	case secretAlnumDash:
		slot = 13
	case secretUpperAlnum:
		slot = 15
	}
	return secretFormat{literal, secretPrefixed, shape, run, min, max, slot}
}

var secretFormats = []secretFormat{
	{literal: "-----BEGIN ", kind: secretKeyBlock},
	{literal: "://", kind: secretAddress},
	prefixedSecret("sk-", secretSK, secretKey, 20, 0),
	prefixedSecret("AIza", secretGoogle, secretKey, 35, 0),
	prefixedSecret("ghp_", secretRun, secretAlnum, 36, 0),
	prefixedSecret("gho_", secretRun, secretAlnum, 36, 0),
	prefixedSecret("ghu_", secretRun, secretAlnum, 36, 0),
	prefixedSecret("ghs_", secretRun, secretAlnum, 36, 0),
	prefixedSecret("ghr_", secretRun, secretAlnum, 36, 0),
	prefixedSecret("github_pat_", secretRun, secretWord, 22, 0),
	prefixedSecret("glpat-", secretRun, secretKey, 20, 0),
	prefixedSecret("npm_", secretRun, secretAlnum, 36, 0),
	prefixedSecret("hf_", secretRun, secretAlnum, 30, 0),
	prefixedSecret("AKIA", secretBounded, secretUpperAlnum, 16, 16),
	prefixedSecret("ASIA", secretBounded, secretUpperAlnum, 16, 16),
	prefixedSecret("LTAI", secretBounded, secretAlnum, 12, 20),
	prefixedSecret("AKID", secretBounded, secretAlnum, 28, 36),
	prefixedSecret("xoxa-", secretRun, secretAlnumDash, 10, 0),
	prefixedSecret("xoxb-", secretRun, secretAlnumDash, 10, 0),
	prefixedSecret("xoxp-", secretRun, secretAlnumDash, 10, 0),
	prefixedSecret("xoxr-", secretRun, secretAlnumDash, 10, 0),
	prefixedSecret("xoxs-", secretRun, secretAlnumDash, 10, 0),
	prefixedSecret("sk_live_", secretRun, secretAlnum, 16, 0),
	prefixedSecret("sk_test_", secretRun, secretAlnum, 16, 0),
	prefixedSecret("rk_live_", secretRun, secretAlnum, 16, 0),
	prefixedSecret("rk_test_", secretRun, secretAlnum, 16, 0),
	prefixedSecret("whsec_", secretRun, secretAlnum, 24, 0),
	prefixedSecret("eyJ", secretJWT, secretKey, 8, 2048),
}

var secretClasses, secretLiteralHeads, secretLiteralOverlap = buildSecretTables()

func secretClass(code int) int {
	if code < 0 || code >= 128 {
		return 0
	}
	return secretClasses[code]
}

func buildSecretTables() (classes [128]int, heads [128][]int, overlap []int) {
	for code := range 128 {
		upper, lower, digit := code >= 'A' && code <= 'Z', code >= 'a' && code <= 'z', code >= '0' && code <= '9'
		alnum := upper || lower || digit
		authority := !((code >= 9 && code <= 13) || code == ' ' || code == '/' || code == '@' || code == '?' || code == '#')
		for _, entry := range []struct {
			ok  bool
			bit int
		}{
			{alnum || code == '_' || code == '-', secretKey},
			{alnum, secretAlnum},
			{alnum || code == '_', secretWord},
			{alnum || code == '-', secretAlnumDash},
			{upper || digit, secretUpperAlnum},
			{upper || digit || code == ' ', secretLabel},
			{lower || digit || code == '+' || code == '.' || code == '-', secretScheme},
			{lower, secretLower},
			{authority && code != ':', secretUser},
			{authority, secretPassword},
		} {
			if entry.ok {
				classes[code] |= entry.bit
			}
		}
	}
	overlap = make([]int, len(secretFormats))
	for index, format := range secretFormats {
		heads[format.literal[0]] = append(heads[format.literal[0]], index)
		for size := len(format.literal) - 1; size > 0 && overlap[index] == 0; size-- {
			suffix := format.literal[len(format.literal)-size:]
			for _, other := range secretFormats {
				if len(other.literal) >= size && other.literal[:size] == suffix {
					overlap[index] = size
					break
				}
			}
		}
	}
	return classes, heads, overlap
}
