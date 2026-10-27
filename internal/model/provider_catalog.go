package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"dsgo/internal/core"
)

// ListModels discovers candidates from the configured endpoint. It never
// persists or automatically activates a returned model on the user's behalf.
func (p *ProviderClient) ListModels(ctx context.Context) ([]ModelInfo, error) {
	key, err := p.credential()
	if err != nil {
		return nil, err
	}
	base := p.transport.root
	if p.options.API == "anthropic-messages" && !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	endpoint := base + "/models"
	if p.options.API == "anthropic-messages" {
		endpoint += "?limit=1000"
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, invalid()
	}
	r.Header = p.headers.Clone()
	r.Header.Set("Accept", "application/json")
	r.Header.Set("User-Agent", "dsgo/0.1 (provider model discovery)")
	if p.options.API == "anthropic-messages" {
		r.Header.Set("x-api-key", key)
		r.Header.Set("anthropic-version", "2023-06-01")
	} else {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	response, err := p.transport.http.Do(r)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{Code: "TRANSPORT"}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, p.transport.responseError(response)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{Code: "TRANSPORT"}
	}
	if len(data) > 2<<20 {
		return nil, &Error{Code: "RESPONSE_LIMIT"}
	}
	return providerCatalog(data)
}

func providerCatalog(data []byte) ([]ModelInfo, error) {
	var envelope struct {
		Data   []json.RawMessage          `json:"data"`
		Models map[string]json.RawMessage `json:"models"`
	}
	if json.Unmarshal(data, &envelope) != nil || envelope.Data == nil && envelope.Models == nil {
		return nil, malformed()
	}
	rows := envelope.Data
	keys := []string{}
	for key := range envelope.Models {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		var entry map[string]json.RawMessage
		if json.Unmarshal(envelope.Models[key], &entry) != nil || entry == nil {
			continue
		}
		entry["id"] = core.JSON(key)
		rows = append(rows, core.JSON(entry))
	}
	seen := map[string]bool{}
	out := []ModelInfo{}
	for _, raw := range rows {
		var entry map[string]json.RawMessage
		if json.Unmarshal(raw, &entry) != nil || entry == nil {
			continue
		}
		var id string
		if json.Unmarshal(entry["id"], &id) != nil || id == "" || len(id) > 512 || seen[id] {
			continue
		}
		seen[id] = true
		info := ModelInfo{ID: id, Name: id, InputModalities: []string{"text"}, Supported: true}
		for _, key := range []string{"name", "display_name"} {
			var value string
			if json.Unmarshal(entry[key], &value) == nil && value != "" {
				info.Name = value
				break
			}
		}
		for _, key := range []string{"context_window", "contextWindow", "max_input_tokens"} {
			var value int
			if json.Unmarshal(entry[key], &value) == nil && value > 0 {
				info.ContextWindow = value
				break
			}
		}
		for _, key := range []string{"max_output_tokens", "max_tokens", "maxTokens"} {
			var value int
			if json.Unmarshal(entry[key], &value) == nil && value > 0 {
				info.MaxTokens = value
				break
			}
		}
		for _, key := range []string{"input_modalities", "input"} {
			var values []string
			if json.Unmarshal(entry[key], &values) == nil && len(values) > 0 {
				info.InputModalities = values
				break
			}
		}
		out = append(out, info)
	}
	return out, nil
}
