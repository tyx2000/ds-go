package tui

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"dsgo/internal/core"
)

type displaySecret struct {
	key  string
	span SecretSpan
}

func userRecordSecrets(r Record) []displaySecret {
	if r.Kind != "user" {
		return nil
	}
	var m core.Message
	if json.Unmarshal(r.Input, &m) != nil || m.Role != "user" {
		return textDisplaySecrets(r.ID, -1, r.Text)
	}
	var out []displaySecret
	for i, b := range m.Content {
		if b.Type == "text" {
			out = append(out, textDisplaySecrets(r.ID, i, b.Text)...)
		}
	}
	return out
}

func textDisplaySecrets(record string, block int, text string) []displaySecret {
	var out []displaySecret
	for _, span := range SentUserSecretSpans(text) {
		// A different value at the same position must not inherit visibility.
		digest := sha256.Sum256([]byte(text[span.Start:span.End]))
		key := fmt.Sprintf("%s/%d/%d/%d/%x", record, block, span.Start, span.End, digest)
		out = append(out, displaySecret{key: key, span: span})
	}
	return out
}

func displayUserText(record string, block int, text string, revealed map[string]bool) string {
	var out strings.Builder
	from := 0
	for _, secret := range textDisplaySecrets(record, block, text) {
		out.WriteString(text[from:secret.span.Start])
		if revealed[secret.key] {
			out.WriteString(text[secret.span.Start:secret.span.End])
		} else {
			out.WriteString(secret.span.Masked)
		}
		from = secret.span.End
	}
	out.WriteString(text[from:])
	return out.String()
}

// displayUserRecord derives a projection. Durable input, copying and Raw retain
// the original bytes; assistant/tool output does not use the user-text rule.
func displayUserRecord(r Record, revealed map[string]bool) Record {
	if r.Kind != "user" {
		return r
	}
	var m core.Message
	if json.Unmarshal(r.Input, &m) != nil || m.Role != "user" {
		r.Text = displayUserText(r.ID, -1, r.Text, revealed)
		return r
	}
	for i := range m.Content {
		if m.Content[i].Type == "text" {
			m.Content[i].Text = displayUserText(r.ID, i, m.Content[i].Text, revealed)
		}
	}
	r.Text, _, _ = messageSummary(m)
	r.Input = core.JSON(m)
	return r
}
