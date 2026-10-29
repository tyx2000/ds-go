package tui

import (
	"strings"
	"testing"

	"dsgo/internal/core"
)

func TestSentUserDisplayKeepsCanonicalInputAndRaw(t *testing.T) {
	secret := "sk-1" + strings.Repeat("a", 39)
	m := core.TextMessage("user", "Use "+secret)
	r := Record{ID: "input1", Kind: "user", Input: core.JSON(m), Text: m.Content[0].Text}
	original := string(r.Input)
	for _, tab := range []string{"summary", "input"} {
		body := strings.Join(Inspector(r, tab), "\n")
		if strings.Contains(body, secret) || !strings.Contains(body, "sk-1aaaa") || !strings.Contains(body, "***") {
			t.Fatalf("tab %s did not mask user text: %s", tab, body)
		}
	}
	if body := strings.Join(Inspector(r, "raw"), "\n"); !strings.Contains(body, secret) {
		t.Fatal("explicit raw view changed")
	}
	if string(r.Input) != original || r.Text != m.Content[0].Text {
		t.Fatal("display mutated the recorded input")
	}
	r.Kind = "assistant"
	if body := strings.Join(Inspector(r, "summary"), "\n"); !strings.Contains(body, secret) {
		t.Fatal("user mask applied to assistant text")
	}
}

func TestSentUserDisplayVisibilityIsPerValue(t *testing.T) {
	first := "sk-1" + strings.Repeat("a", 39)
	second := "sk-2" + strings.Repeat("b", 39)
	m := core.TextMessage("user", first+" "+second)
	r := Record{ID: "input1", Kind: "user", Input: core.JSON(m), Text: m.Content[0].Text}
	p := pager{data: Transcript{Records: []Record{r}}, detail: true}
	p.handle("v")
	body := displayUserRecord(r, p.revealed).Text
	if !strings.Contains(body, first) || strings.Contains(body, second) {
		t.Fatalf("first visibility: %q", body)
	}
	p.handle("V")
	p.handle("v")
	body = displayUserRecord(r, p.revealed).Text
	if !strings.Contains(body, first) || !strings.Contains(body, second) {
		t.Fatalf("independent visibility: %q", body)
	}
	m.Content[0].Text = "sk-3" + strings.Repeat("c", 39) + " " + second
	r.Input = core.JSON(m)
	if strings.Contains(displayUserRecord(r, p.revealed).Text, strings.Repeat("c", 39)) {
		t.Fatal("different value inherited the previous value's visibility")
	}
}
