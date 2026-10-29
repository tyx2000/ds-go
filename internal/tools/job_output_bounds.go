package tools

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"dsgo/internal/core"
)

// JobOutputLimit is visible only to the job's owner. A producer that omits its
// outputLimitBytes supplies no cap: the jobs controller adds no global default.
func (r *Registry) JobOutputLimit(owner, id string) (int, bool) {
	r.mu.RLock()
	j := r.jobs[id]
	r.mu.RUnlock()
	if j == nil || owner == "" || j.owner != owner {
		return 0, false
	}
	j.mu.Lock()
	limit := j.outputLimitBytes
	j.mu.Unlock()
	return limit, limit > 0
}

// jobResult retains the full, execution-local canonical value for PTC. The
// presentation may be bounded later without corrupting the RPC's JSON value.
func jobResult(value any) core.ToolResult {
	raw := core.JSON(value)
	return core.ToolResult{Value: raw, Content: string(raw)}
}

func retainJobHead(text string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(text) <= maxBytes {
		return text
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end]
}

func retainJobTail(text string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(text) <= maxBytes {
		return text
	}
	start := len(text) - maxBytes
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[start:]
}

func fitJobSuffix(content, suffix string, maxBytes int, omitted string) string {
	complete := content + suffix
	if maxBytes <= 0 || len(complete) <= maxBytes {
		return complete
	}
	fixed := suffix
	if !strings.HasSuffix(content, strings.TrimLeft(omitted, " \t\n\r\v\f")) {
		fixed = omitted + suffix
	}
	if len(fixed) >= maxBytes {
		return retainJobTail(fixed, maxBytes)
	}
	return retainJobTail(content, maxBytes-len(fixed)) + fixed
}

// BoundJobText mirrors the pinned tool-jobs single-text finalizer. The caller
// supplies only the model projection; the canonical value is never truncated.
func BoundJobText(text string, maxBytes int) string {
	return fitJobSuffix(text, "", maxBytes, "\n[result truncated]")
}

// BoundJobOutput preserves the status suffix only if policy left the default
// rendering of this canonical job_output value intact.
func BoundJobOutput(text string, canonical json.RawMessage, maxBytes int) string {
	var value struct {
		Text *string   `json:"text"`
		Job  publicJob `json:"job"`
	}
	if json.Unmarshal(canonical, &value) == nil && value.Text != nil {
		body := *value.Text
		if body == "" {
			body = "(no new output)"
		}
		body = strings.TrimSuffix(body, "\n")
		suffix := "\n" + statusLine(value.Job)
		if text == body+suffix {
			return fitJobSuffix(body, suffix, maxBytes, "\n[output truncated]")
		}
	}
	return BoundJobText(text, maxBytes)
}

func fitCompletionNotice(id, kind, label, line string, maxBytes int) string {
	prefix := "background job " + id
	detail := " (" + kind + ": " + label + ") finished " + line
	action := "\nDone; job_output."
	complete := prefix + detail + ". Read its output with job_output."
	if maxBytes <= 0 || len(complete) <= maxBytes {
		return complete
	}
	omitted := "\n[notice truncated]"
	fixed := prefix + omitted + action
	if len(fixed) <= maxBytes {
		return prefix + retainJobHead(detail, maxBytes-len(fixed)) + omitted + action
	}
	if compact := prefix + action; len(compact) <= maxBytes {
		return compact
	}
	if recovery := "background job ended; use job_list for id."; len(recovery) <= maxBytes {
		return recovery
	}
	return retainJobHead("job_list", maxBytes)
}
