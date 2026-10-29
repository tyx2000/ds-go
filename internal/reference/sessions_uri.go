package reference

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const SessionScheme = "dsh-session:"

type SessionInput struct {
	SessionID string `json:"sessionId"`
	Label     string `json:"label"`
}

func EncodeSessionURI(id string) string {
	return SessionScheme + base64.RawURLEncoding.EncodeToString(sessionJSONString(id))
}

func DecodeSessionURI(uri string) (string, error) {
	invalid := func() (string, error) {
		return "", &SessionError{Code: "SESSION_REFERENCE_INVALID_REFERENCE", Message: "invalid session reference URI"}
	}
	if !strings.HasPrefix(uri, SessionScheme) {
		return invalid()
	}
	payload := strings.TrimPrefix(uri, SessionScheme)
	if payload == "" {
		return invalid()
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(payload)
	if err != nil || !utf8.Valid(data) || !json.Valid(data) || len(data) < 2 || data[0] != '"' || data[len(data)-1] != '"' {
		return invalid()
	}
	id, err := decodeSessionJSONString(data)
	if err != nil || EncodeSessionURI(id) != uri {
		return invalid()
	}
	return id, nil
}

func FormatSessionMention(id string, labels ...string) string {
	label := id
	if len(labels) > 0 {
		label = labels[0]
	}
	label = strings.NewReplacer(`\`, `\\`, `]`, `\]`).Replace(label)
	return "@[" + label + "](" + EncodeSessionURI(id) + ")"
}

// Match the official markdown alternative before bare URI tokens. A scheme
// without a base64url-shaped payload remains ordinary discussion text.
var sessionMentionPattern = regexp.MustCompile(`@\[((?:\\[^\r\n\x{2028}\x{2029}]|[^\\\]])*)\]\((dsh-session:[^\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff})]*)\)|(dsh-session:[A-Za-z0-9_-]+)`)

func ParseSessionText(text string) (string, []SessionInput, error) {
	var out strings.Builder
	references := []SessionInput{}
	last := 0
	for _, match := range sessionMentionPattern.FindAllStringSubmatchIndex(text, -1) {
		out.WriteString(text[last:match[0]])
		uri := ""
		if match[4] >= 0 {
			uri = text[match[4]:match[5]]
		} else {
			uri = text[match[6]:match[7]]
		}
		id, err := DecodeSessionURI(uri)
		if err != nil {
			return "", nil, err
		}
		label := id
		if match[2] >= 0 {
			label = unescapeSessionLabel(text[match[2]:match[3]])
		}
		references = append(references, SessionInput{SessionID: id, Label: label})
		out.WriteString("@" + label)
		last = match[1]
	}
	out.WriteString(text[last:])
	return out.String(), references, nil
}

func unescapeSessionLabel(label string) string {
	var out strings.Builder
	escaped := false
	for _, r := range label {
		if !escaped && r == '\\' {
			escaped = true
			continue
		}
		out.WriteRune(r)
		escaped = false
	}
	return out.String()
}

// JSON.stringify uses lossless escapes for lone UTF-16 surrogates. Go strings
// can represent these opaque ids as WTF-8 bytes, so the codec preserves them
// without letting encoding/json replace them with U+FFFD.
func sessionJSONString(value string) []byte {
	var out strings.Builder
	out.WriteByte('"')
	for len(value) > 0 {
		r, size := utf8.DecodeRuneInString(value)
		if r == utf8.RuneError && size == 1 && len(value) >= 3 && value[0] == 0xed && value[1] >= 0xa0 && value[1] <= 0xbf && value[2]&0xc0 == 0x80 {
			r = rune(value[0]&15)<<12 | rune(value[1]&63)<<6 | rune(value[2]&63)
			size = 3
		}
		value = value[size:]
		switch r {
		case '"', '\\':
			out.WriteByte('\\')
			out.WriteRune(r)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if r < 0x20 || r >= 0xd800 && r <= 0xdfff {
				fmt.Fprintf(&out, `\u%04x`, r)
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
	return []byte(out.String())
}

func decodeSessionJSONString(data []byte) (string, error) {
	var out strings.Builder
	for i := 1; i < len(data)-1; i++ {
		if data[i] != '\\' {
			out.WriteByte(data[i])
			continue
		}
		i++
		switch data[i] {
		case '"', '\\', '/':
			out.WriteByte(data[i])
		case 'b':
			out.WriteByte('\b')
		case 'f':
			out.WriteByte('\f')
		case 'n':
			out.WriteByte('\n')
		case 'r':
			out.WriteByte('\r')
		case 't':
			out.WriteByte('\t')
		case 'u':
			unit, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
			if err != nil {
				return "", err
			}
			i += 4
			r := rune(unit)
			if r >= 0xd800 && r <= 0xdbff && i+6 < len(data) && data[i+1] == '\\' && data[i+2] == 'u' {
				low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
				if err == nil && low >= 0xdc00 && low <= 0xdfff {
					out.WriteRune(utf16.DecodeRune(r, rune(low)))
					i += 6
					continue
				}
			}
			if r >= 0xd800 && r <= 0xdfff {
				out.Write([]byte{byte(0xe0 | r>>12), byte(0x80 | r>>6&63), byte(0x80 | r&63)})
			} else {
				out.WriteRune(r)
			}
		}
	}
	return out.String(), nil
}
