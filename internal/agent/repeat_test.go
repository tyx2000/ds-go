package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRepeatReminderExactThresholdsCanonicalArgumentsAndAgentIsolation(t *testing.T) {
	r, err := NewRepeatReminder(DefaultRepeatConfig())
	if err != nil {
		t.Fatal(err)
	}
	for count := 1; count <= 10; count++ {
		raw := json.RawMessage(`{"b":[1,{"y":true,"x":-0}],"a":1.0}`)
		if count%2 == 0 {
			raw = json.RawMessage(`{"a":1e0,"b":[1,{"x":0,"y":true}]}`)
		}
		notice := r.Observe("a", "read_file", raw)
		if expected := count == 3 || count == 5 || count == 8; (notice != nil) != expected {
			t.Fatalf("count %d notice=%#v", count, notice)
		}
		if notice != nil {
			if notice.Source.Kind != "repeat-tool-reminder" || notice.Source.Form != "notice" {
				t.Fatalf("missing source: %#v", notice)
			}
			if count == 3 && !strings.HasPrefix(notice.Text, "You are repeating the exact same tool call") {
				t.Fatalf("first reminder=%s", notice.Text)
			}
			if count > 3 && !strings.Contains(notice.Text, `- arguments: {"a":1,"b":[1,{"x":0,"y":true}]}`) {
				t.Fatalf("detailed reminder=%s", notice.Text)
			}
		}
		if other := r.Observe("b", "different", json.RawMessage(`{"n":1}`)); other != nil && count != 3 && count != 5 && count != 8 {
			t.Fatalf("agents shared a chain: %#v", other)
		}
	}
	r.Reset("a")
	if notice := r.Observe("a", "read_file", json.RawMessage(`{"a":1,"b":[1,{"x":0,"y":true}]}`)); notice != nil {
		t.Fatalf("user reset retained chain: %#v", notice)
	}
}

func TestRepeatReminderTransparentFiltersAndPreview(t *testing.T) {
	config := DefaultRepeatConfig()
	config.Thresholds = []int{4, 2}
	config.Include = []string{"read_*"}
	config.Exclude = []string{"read_skip"}
	config.ArgumentsPreviewChars = 12
	r, _ := NewRepeatReminder(config)
	input := json.RawMessage(`{"query":"abcdefghi"}`)
	if r.Observe("a", "read_file", input) != nil {
		t.Fatal("first call reminded")
	}
	r.Observe("a", "todo_write", json.RawMessage(`{}`))
	r.Observe("a", "read_skip", json.RawMessage(`{}`))
	if notice := r.Observe("a", "read_file", input); notice == nil || !strings.HasPrefix(notice.Text, "You are repeating") {
		t.Fatalf("transparent filters reset chain: %#v", notice)
	}
	r.Observe("a", "read_file", input)
	if notice := r.Observe("a", "read_file", input); notice == nil || !strings.Contains(notice.Text, "… (+") {
		t.Fatalf("preview not bounded: %#v", notice)
	}
	r.Observe("a", "read_other", input)
	if r.Observe("a", "read_file", input) != nil {
		t.Fatal("different tracked call did not reset chain")
	}
	for i := 0; i < 10; i++ {
		if r.Observe("", "read_file", input) != nil {
			t.Fatal("non-agent call reminded")
		}
	}
}

func TestRepeatCanonicalMatchesJavaScriptJSONDomain(t *testing.T) {
	for raw, want := range map[string]string{
		`{"10":1,"2":2,"01":3,"b":4,"a":5}`:                         `{"2":2,"10":1,"01":3,"a":5,"b":4}`,
		`{"large":9007199254740993,"overflow":1e400,"negative":-0}`: `{"large":9007199254740992,"negative":0,"overflow":null}`,
		`{"\ue000":1,"😀":2}`:                                        `{"😀":2,"":1}`,
		`<bad`:                                                      `"<bad"`,
		`{"x":"<tag>&\u2028"}`:                                      "{\"x\":\"<tag>&\u2028\"}",
	} {
		if got := canonicalArguments(json.RawMessage(raw)); got != want {
			t.Errorf("canonical %s => %s, want %s", raw, got, want)
		}
	}
}

func TestRepeatConfigurationFailsLoud(t *testing.T) {
	for _, config := range []RepeatConfig{{ArgumentsPreviewChars: 500}, {Thresholds: []int{1}, ArgumentsPreviewChars: 500}, {Thresholds: []int{3, 3}, ArgumentsPreviewChars: 500}, {Thresholds: []int{3}, ArgumentsPreviewChars: 0}} {
		if _, err := NewRepeatReminder(config); err == nil {
			t.Fatalf("accepted invalid policy %#v", config)
		}
	}
}
