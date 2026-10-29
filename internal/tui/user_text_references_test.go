package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSentUserReferencesOwnOnlyOfficialSpelling(t *testing.T) {
	token := "sk-1" + strings.Repeat("a", 22)
	masked := token[:8] + strings.Repeat("*", len(token)-8)
	for _, tc := range []struct{ text, want string }{
		{"@" + token, "@" + token},
		{"x@" + token, "x@" + masked},
		{"看@" + token, "看@" + masked},
		{"看 @" + token + "！", "看 @" + token + "！"},
		{"@\"directory/" + token + " with spaces\"", "@\"directory/" + token + " with spaces\""},
		{"x@[label " + token + "](dsh-session:opaque)", "x@[label " + token + "](dsh-session:opaque)"},
		{"/tmp/" + token, "/tmp/" + masked},
		{"https://host/" + token, "https://host/" + masked},
		{"[ordinary URL](https://user:password@host)", "[ordinary URL](https://user:********@host)"},
		{"dsh-session:" + token, "dsh-session:" + masked},
		{"@http://user:" + token + "@host", "@http://user:" + token + "@host"},
		{"x@http://user:" + token + "@host", "x@http://user:" + strings.Repeat("*", len(token)) + "@host"},
		{"@\"a " + token + " http://user\":password@host", "@\"a " + token + " http://user\":********@host"},
	} {
		if got := MaskSentUserText(tc.text); got != tc.want {
			t.Fatalf("reference selection:\n got %q\nwant %q", got, tc.want)
		}
	}
}

func TestSentUserReferencesUseConfirmedFactsAndLongestLabel(t *testing.T) {
	token := "sk-1" + strings.Repeat("b", 22)
	label := "release " + token
	text := "看@" + label + " later"
	if got := MaskSentUserText(text, "release", label); got != text {
		t.Fatalf("longest known session label was not selected: %q", got)
	}
	if got := MaskSentUserText(text, "release"); got == text {
		t.Fatal("short session label suppressed the following secret")
	}
	text = "/" + token + " objective " + token
	for _, facts := range []SentUserTextFacts{
		{SkillNames: []string{token}},
		{Commands: []SentUserCommand{{Start: 0, End: len(token) + 1, Name: token}}},
	} {
		got := MaskSentUserTextWithFacts(text, facts)
		if !strings.HasPrefix(got, "/"+token+" objective ") || strings.HasSuffix(got, token) {
			t.Fatalf("confirmed slash reference hid arguments or lost its own range: %q", got)
		}
	}
	if got := MaskSentUserText(text); strings.HasPrefix(got, "/"+token) {
		t.Fatal("unconfirmed slash spelling acquired a reference")
	}
}

func TestSentUserPathGrammarPunctuationQuotesAndWhitespace(t *testing.T) {
	for _, tc := range []struct {
		text       string
		start, end int
		found      bool
	}{
		{"@file!!!", 0, 5, true},
		{"@\"file!\"tail", 0, 8, true},
		{"@\"unfinished name", 0, 12, true},
		{"@\"\"", 0, 3, true},
		{"@!?", 0, 0, false},
		{"x@file", 0, 0, false},
		{"中@file", 0, 0, false},
		{"\u00a0@file/", 2, 8, true},
		{"\u0085@file", 0, 0, false},
	} {
		span, ok := nextSentPathReference(tc.text, 0)
		if ok != tc.found || ok && (span.start != tc.start || span.end != tc.end) {
			t.Fatalf("path grammar %q: %+v %v", tc.text, span, ok)
		}
	}
}

type sentUserGoldenCase struct {
	Text  string            `json:"text"`
	Facts SentUserTextFacts `json:"facts"`
}

func TestSentUserUpstreamDifferential(t *testing.T) {
	root := os.Getenv("DSGO_OFFICIAL_SRC")
	if root == "" {
		root = "/private/tmp/dsgo-official-687f282"
	}
	if _, err := os.Stat(filepath.Join(root, "packages/client/ui-conversation/src/client/conversation/user-text-document.ts")); os.IsNotExist(err) {
		t.Skip("pinned official source checkout is unavailable")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is unavailable for pure-source differential")
	}
	token := "sk-1" + strings.Repeat("a", 24)
	block := "-----BEGIN PRIVATE KEY-----\n@file " + token + "\n-----END PRIVATE KEY-----"
	var cases []sentUserGoldenCase
	add := func(text string, facts SentUserTextFacts) { cases = append(cases, sentUserGoldenCase{text, facts}) }
	for _, prefix := range []string{"", "x", "中", "😀", " ", "\u00a0", "\u0085", "\u2028"} {
		for _, reference := range []string{
			"@" + token, "@\"" + token + " spaces\"", "@\"" + token + " spaces", "@[label " + token + "](dsh-session:x)",
			"@[" + token + "]()", "@\"http://user\":password@host", "@\"" + token + " http://user\":password@host", "@" + token + "?", "@" + token + "/", "@" + token + "！", "@\"\"", "@?", "@label " + token,
		} {
			for _, facts := range []SentUserTextFacts{{}, {SessionLabels: []string{"label", "label " + token}}, {SessionLabels: []string{token}}} {
				add(prefix+reference, facts)
			}
		}
	}
	for _, text := range []string{block, token + block, "@[label\n" + token + "](dsh-session:x)", "@[label](dsh-session:)", "@[label](dsh-session:" + token + ")", "http://user:password@host @file " + token, "/" + token + " " + token} {
		add(text, SentUserTextFacts{})
		add(text, SentUserTextFacts{SkillNames: []string{token}, Commands: []SentUserCommand{{0, len(token) + 1, token}}})
	}
	random := rand.New(rand.NewSource(0x726566))
	pieces := []string{"", " ", "\n", "\r\n", "😀", "中", "@", "@\"", "@file/", "@\"file ", "@label", "@label " + token, "@[label](dsh-session:x)", "](dsh-session:x)", "http://user:", "password@host", token, block, "/" + token, "?", "！", "/", "\u00a0", "\u0085"}
	for index := 0; index < 500; index++ {
		var text strings.Builder
		for count := 2 + random.Intn(8); count > 0; count-- {
			text.WriteString(pieces[random.Intn(len(pieces))])
		}
		add(text.String(), SentUserTextFacts{SessionLabels: []string{"label", "label " + token}, SkillNames: []string{token}})
	}
	raw, err := json.Marshal(map[string]any{"root": root, "cases": cases})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--no-warnings", "--experimental-transform-types", "--input-type=module", "--eval", sentUserDifferentialProgram)
	cmd.Stdin = bytes.NewReader(raw)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("pure-source differential: %v\n%s", err, stderr.String())
	}
	var expected [][]SecretSpan
	if err := json.Unmarshal(output, &expected); err != nil || len(expected) != len(cases) {
		t.Fatalf("invalid golden output: %v", err)
	}
	for index, tc := range cases {
		got := SentUserSecretSpansWithFacts(tc.Text, tc.Facts)
		if len(got) != len(expected[index]) {
			t.Fatalf("case %d: got %+v want %+v; text %q facts %+v", index, got, expected[index], tc.Text, tc.Facts)
		}
		for i, span := range got {
			if span != expected[index][i] {
				t.Fatalf("case %d span %d: got %+v want %+v; text %q facts %+v", index, i, span, expected[index][i], tc.Text, tc.Facts)
			}
		}
	}
}

const sentUserDifferentialProgram = `
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
const input = JSON.parse(readFileSync(0, 'utf8'));
const load = async path => import(pathToFileURL(join(input.root, path)).href);
const { UserTextDocument } = await load('packages/client/ui-conversation/src/client/conversation/user-text-document.ts');
const { sessionReferenceRule } = await load('packages/client/ui-conversation/src/client/user-text/session-reference-rule.ts');
const { fileReferenceRule, folderReferenceRule } = await load('packages/client/ui-reference/src/client/user-text-rules.ts');
const { goalCommandReferenceRule } = await load('packages/client/ui-goal/src/client/user-text-rules.ts');
const { skillReferenceRule } = await load('packages/client/ui-skill/src/client/user-text-rules.ts');
const { createSecretRule } = await load('packages/client/ui-secret-mask/src/client/secret-rule.ts');
const rules = [createSecretRule(), sessionReferenceRule, fileReferenceRule, folderReferenceRule, goalCommandReferenceRule, skillReferenceRule]
  .sort((a,b) => a.order - b.order || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
const offset = (text, at) => Buffer.byteLength(text.slice(0, at), 'utf8');
const results = input.cases.map(row => {
  const facts = { ...row.facts, commands: row.facts.commands?.map(command => ({ ...command,
    start: Buffer.from(row.text).subarray(0, command.start).toString('utf8').length,
    end: Buffer.from(row.text).subarray(0, command.end).toString('utf8').length,
  })) };
  return new UserTextDocument().evaluate({text:row.text, facts}, rules)
    .filter(node => node.id === 'secret')
    .map(node => ({start:offset(row.text,node.start), end:offset(row.text,node.end), masked:node.data.masked}));
});
process.stdout.write(JSON.stringify(results));
`
