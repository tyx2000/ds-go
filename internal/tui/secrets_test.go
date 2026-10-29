package tui

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSecretMaskPublishedFormatsAndPlainAssignments(t *testing.T) {
	for _, format := range secretFormats {
		if format.kind != secretPrefixed {
			continue
		}
		body := strings.Repeat("a", format.min)
		if format.run == secretUpperAlnum {
			body = strings.Repeat("A", format.min)
		}
		if format.shape == secretSK {
			body = "1" + body[1:]
		}
		if format.shape == secretJWT {
			body += ".eyJ" + strings.Repeat("b", 8) + "." + strings.Repeat("c", 8)
		}
		token := format.literal + body
		t.Run(format.literal, func(t *testing.T) {
			text := "before " + token + " after"
			spans := SecretSpans(text)
			if len(spans) != 1 || spans[0].Start != 7 || spans[0].End != 7+len(token) || spans[0].Masked != token[:8]+strings.Repeat("*", len(token)-8) {
				t.Fatalf("published format not masked: %+v", spans)
			}
			if MaskUserText(text) != "before "+spans[0].Masked+" after" || text != "before "+token+" after" {
				t.Fatal("display changed original text")
			}
		})
	}
	for _, text := range []string{
		"password=ordinary-password api_key=not-a-published-key",
		"sk-" + strings.Repeat("a", 1000),
		"xghp_" + strings.Repeat("a", 36),
		"ghp_" + strings.Repeat("a", 35),
		"AKIA" + strings.Repeat("A", 17),
		"AKIA" + strings.Repeat("A", 16) + "_",
		"http://user:@host",
		"http://user:password?query@host",
		"HTTP://user:password@host",
		"x://user:password@host",
		"-----BEGIN CERTIFICATE-----\npublic\n-----END CERTIFICATE-----",
	} {
		if got := MaskUserText(text); got != text {
			t.Fatalf("plain text was masked: %q -> %q", text, got)
		}
	}
}

func TestSecretAddressPasswordAndByteOffsets(t *testing.T) {
	text := "前😀 http://用户:密😀码:另一个@host/path"
	start := strings.Index(text, "密")
	end := strings.Index(text, "@")
	spans := SecretSpans(text)
	if len(spans) != 1 || spans[0] != (SecretSpan{start, end, "*******"}) {
		t.Fatalf("address password or UTF-8 offsets: %+v", spans)
	}
	for _, from := range []int{0, strings.Index(text, "://") + 1, strings.Index(text, "用户"), start} {
		span, ok := FindSecretSpan(text, from)
		if !ok || span != spans[0] {
			t.Fatalf("address look-behind at %d: %+v %v", from, span, ok)
		}
	}
	// Queries after a credential begins must not rediscover that earlier range.
	if span, ok := FindSecretSpan(text, start+len("密")); ok {
		t.Fatalf("found a password beginning before the query: %+v", span)
	}
	for _, units := range []int{256, 258} {
		username := strings.Repeat("😀", units/2)
		text := "https://" + username + ":pass@host"
		if got := len(SecretSpans(text)); (units == 256 && got != 1) || (units == 258 && got != 0) {
			t.Fatalf("username %d UTF-16 units: %d spans", units, got)
		}
	}
}

func TestSecretPrivateKeyLinesAndOverlap(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"-----BEGIN PRIVATE KEY-----\nabc😀\r\n秘密\n-----END RSA PRIVATE KEY-----", "-----BEGIN PRIVATE KEY-----\n****\r\n**\n-----END RSA PRIVATE KEY-----"},
		{"-----BEGIN PRIVATE KEY-----\nabc\nunfinished😀", "-----BEGIN PRIVATE KEY-----\n***\n***********"},
		{"-----BEGIN PRIVATE KEY-----secret-----END PRIVATE KEY-----", strings.Repeat("*", len("-----BEGIN PRIVATE KEY-----secret-----END PRIVATE KEY-----"))},
	} {
		if got := MaskUserText(tc.text); got != tc.want || !utf8.ValidString(got) {
			t.Fatalf("key block mismatch:\n got %q\nwant %q", got, tc.want)
		}
	}
	token := "sk-1" + strings.Repeat("a", 20)
	address := "https://name:" + token + "@host"
	if got := MaskUserText(address); got != "https://name:"+strings.Repeat("*", len(token))+"@host" {
		t.Fatalf("same-start address must hide token prefix: %q", got)
	}
	text := token + "-----BEGIN PRIVATE KEY-----\nsecret\n-----END PRIVATE KEY-----"
	spans := SecretSpans(text)
	if len(spans) != 1 || spans[0].Start != 0 || spans[0].End != len(text) || !strings.HasPrefix(spans[0].Masked, token[:8]) || strings.Contains(spans[0].Masked, "BEGIN") || strings.Count(spans[0].Masked, "\n") != 2 {
		t.Fatalf("transitive overlapping token/block not joined: %+v", spans)
	}
}

func TestSecretSKDigitAndJWTBounds(t *testing.T) {
	for _, at := range []int{255, 256} {
		text := "sk-" + strings.Repeat("a", at) + "1"
		if got := len(SecretSpans(text)); (at == 255 && got != 1) || (at == 256 && got != 0) {
			t.Fatalf("digit at %d: %d spans", at, got)
		}
	}
	for _, header := range []int{2048, 2049} {
		text := "eyJ" + strings.Repeat("a", header) + ".eyJbbbbbbbb.cccccccc"
		if got := len(SecretSpans(text)); (header == 2048 && got != 1) || (header == 2049 && got != 0) {
			t.Fatalf("JWT header %d: %d spans", header, got)
		}
	}
}

// The optional fixture is generated directly by the pinned pure TypeScript
// modules. No browser or third-party UI dependencies are involved.
func TestSecretUpstreamGolden(t *testing.T) {
	path := os.Getenv("DSGO_SECRET_GOLDEN")
	if path == "" {
		path = "testdata/secrets_upstream.json"
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skip("run secrets_upstream_generate.mjs to generate the pinned-source differential fixture")
	}
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		SHA   string `json:"sha"`
		Cases []struct {
			Text   string       `json:"text"`
			From   int          `json:"from"`
			First  *SecretSpan  `json:"first"`
			Spans  []SecretSpan `json:"spans"`
			Masked string       `json:"masked"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil || fixture.SHA != "687f282e8625c6b8d11ff58211a90524be2e4317" || len(fixture.Cases) == 0 {
		t.Fatalf("invalid fixture: %v", err)
	}
	for index, tc := range fixture.Cases {
		span, ok := FindSecretSpan(tc.Text, tc.From)
		if ok != (tc.First != nil) || (ok && span != *tc.First) {
			t.Fatalf("case %d query at %d: got %+v %v, want %+v; text %q", index, tc.From, span, ok, tc.First, tc.Text)
		}
		spans := SecretSpans(tc.Text)
		if len(spans) != len(tc.Spans) {
			t.Fatalf("case %d: got %d spans, want %d; text %q", index, len(spans), len(tc.Spans), tc.Text)
		}
		for i := range spans {
			if spans[i] != tc.Spans[i] {
				t.Fatalf("case %d span %d: got %+v want %+v; text %q", index, i, spans[i], tc.Spans[i], tc.Text)
			}
		}
		if got := MaskUserText(tc.Text); got != tc.Masked {
			t.Fatalf("case %d masked: got %q want %q", index, got, tc.Masked)
		}
	}
}
