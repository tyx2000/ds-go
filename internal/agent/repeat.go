package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf16"
)

// RepeatConfig is the official advisory repeat-tool-reminder policy. A caller
// selecting defaults should pass DefaultRepeatConfig; explicit invalid settings
// fail instead of silently falling back. No reminder vetoes tool execution.
type RepeatConfig struct {
	Thresholds            []int    `json:"thresholds"`
	Include               []string `json:"include"`
	Exclude               []string `json:"exclude"`
	ArgumentsPreviewChars int      `json:"argumentsPreviewChars"`
}

func DefaultRepeatConfig() RepeatConfig {
	return RepeatConfig{Thresholds: []int{3, 5, 8}, Include: []string{}, Exclude: []string{}, ArgumentsPreviewChars: 500}
}

type RepeatNotice struct {
	Text   string       `json:"text"`
	Source RepeatSource `json:"source"`
}

type RepeatSource struct {
	Kind    string `json:"kind"`
	Form    string `json:"form"`
	Summary string `json:"summary"`
}

type repeatChain struct {
	key   string
	count int
}

// RepeatReminder owns only in-memory per-agent chains. Restarted engines begin
// fresh; compaction alone must not reset a chain. Reset is for real user prompts.
type RepeatReminder struct {
	mu         sync.Mutex
	chains     map[string]repeatChain
	thresholds map[int]bool
	first      int
	include    []*regexp.Regexp
	exclude    []*regexp.Regexp
	preview    int
}

func NewRepeatReminder(config RepeatConfig) (*RepeatReminder, error) {
	if len(config.Thresholds) == 0 {
		return nil, errors.New("repeat-tool-reminder: `thresholds` must not be empty")
	}
	if config.ArgumentsPreviewChars < 1 {
		return nil, fmt.Errorf("repeat-tool-reminder: invalid argumentsPreviewChars %d — must be an integer >= 1", config.ArgumentsPreviewChars)
	}
	r := &RepeatReminder{chains: map[string]repeatChain{}, thresholds: map[int]bool{}, first: math.MaxInt, preview: config.ArgumentsPreviewChars}
	for _, value := range config.Thresholds {
		if value < 2 {
			return nil, fmt.Errorf("repeat-tool-reminder: invalid threshold %d — every threshold must be an integer >= 2", value)
		}
		if r.thresholds[value] {
			return nil, errors.New("repeat-tool-reminder: `thresholds` must not contain duplicates")
		}
		r.thresholds[value] = true
		if value < r.first {
			r.first = value
		}
	}
	compile := func(patterns []string) []*regexp.Regexp {
		compiled := make([]*regexp.Regexp, 0, len(patterns))
		for _, pattern := range patterns {
			compiled = append(compiled, regexp.MustCompile("(?s)^"+strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*")+"$"))
		}
		return compiled
	}
	r.include, r.exclude = compile(config.Include), compile(config.Exclude)
	return r, nil
}

func (r *RepeatReminder) Reset(agentID string) {
	r.mu.Lock()
	delete(r.chains, agentID)
	r.mu.Unlock()
}

// Observe runs after every tool attempt, including denied or failed attempts.
// Callers must append returned notices after the step's tool results and retain
// their Source metadata. Empty agent IDs represent non-agent callers and do not
// participate. Untracked tools are transparent to an existing repeat chain.
func (r *RepeatReminder) Observe(agentID, name string, input json.RawMessage) *RepeatNotice {
	if agentID == "" {
		return nil
	}
	match := func(patterns []*regexp.Regexp) bool {
		for _, pattern := range patterns {
			if pattern.MatchString(name) {
				return true
			}
		}
		return false
	}
	if len(r.include) > 0 && !match(r.include) || match(r.exclude) {
		return nil
	}
	canonical := canonicalArguments(input)
	key := name + "\x00" + canonical
	r.mu.Lock()
	chain := r.chains[agentID]
	if chain.key == key {
		chain.count++
	} else {
		chain = repeatChain{key: key, count: 1}
	}
	r.chains[agentID] = chain
	r.mu.Unlock()
	if !r.thresholds[chain.count] {
		return nil
	}
	text := "You are repeating the exact same tool call with identical arguments. Carefully analyze the previous result before calling again: if the task is not complete, try a different approach or different arguments instead of repeating the call."
	if chain.count != r.first {
		text = fmt.Sprintf("Repeated tool call detected:\n- tool: %s\n- consecutive_calls: %d\n- arguments: %s\nThe repeated calls are not making progress. Do not call this tool with these exact arguments again. Inspect the latest result and choose a different action, different arguments, or finish the task if enough evidence has been gathered.", name, chain.count, previewArguments(canonical, r.preview))
	}
	return &RepeatNotice{Text: text, Source: RepeatSource{Kind: "repeat-tool-reminder", Form: "notice", Summary: fmt.Sprintf("%s × %d", name, chain.count)}}
}

func canonicalArguments(input json.RawMessage) string {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	err := decoder.Decode(&value)
	var extra any
	if err != nil || decoder.Decode(&extra) != io.EOF {
		// Official malformed-JSON fallback is the raw string, not an unquoted
		// sequence that could accidentally match a parsed object.
		value = string(input)
	}
	var out strings.Builder
	writeCanonicalJSON(&out, value)
	return out.String()
}

func writeCanonicalJSON(out *strings.Builder, value any) {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return jsPropertyLess(keys[i], keys[j]) })
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			out.WriteString(jsJSONString(key))
			out.WriteByte(':')
			writeCanonicalJSON(out, v[key])
		}
		out.WriteByte('}')
	case []any:
		out.WriteByte('[')
		for i, element := range v {
			if i > 0 {
				out.WriteByte(',')
			}
			writeCanonicalJSON(out, element)
		}
		out.WriteByte(']')
	case string:
		out.WriteString(jsJSONString(v))
	case json.Number:
		number, _ := strconv.ParseFloat(string(v), 64)
		if math.IsInf(number, 0) || math.IsNaN(number) {
			out.WriteString("null")
		} else if number == 0 {
			out.WriteByte('0')
		} else {
			b, _ := json.Marshal(number)
			out.Write(b)
		}
	default:
		b, _ := json.Marshal(v)
		out.Write(b)
	}
}

func jsJSONString(value string) string {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	return strings.NewReplacer(`\u2028`, "\u2028", `\u2029`, "\u2029").Replace(strings.TrimSuffix(out.String(), "\n"))
}

// JavaScript objects enumerate array-index keys before other properties, even
// after a deep lexicographic key sort. Non-index keys sort by UTF-16 code unit.
func jsPropertyLess(a, b string) bool {
	index := func(value string) (uint64, bool) {
		i, err := strconv.ParseUint(value, 10, 32)
		return i, err == nil && i < 4294967295 && strconv.FormatUint(i, 10) == value
	}
	ai, ax := index(a)
	bi, bx := index(b)
	if ax != bx {
		return ax
	}
	if ax {
		return ai < bi
	}
	au, bu := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(au) && i < len(bu); i++ {
		if au[i] != bu[i] {
			return au[i] < bu[i]
		}
	}
	return len(au) < len(bu)
}

func previewArguments(canonical string, cap int) string {
	units := utf16.Encode([]rune(canonical))
	if len(units) <= cap {
		return canonical
	}
	return fmt.Sprintf("%s… (+%d more chars)", string(utf16.Decode(units[:cap])), len(units)-cap)
}
