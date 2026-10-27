package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// BaselineVersion is the upstream protocol capability ceiling, not dsgo's
// product identity or a claim of live account/model availability.
const BaselineVersion = "0.2.1-rc.2"

type ModelInfo struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name,omitempty"`
	ContextWindow      int      `json:"context_window,omitempty"`
	MaxTokens          int      `json:"max_tokens,omitempty"`
	InputModalities    []string `json:"input_modalities"`
	SystemPromptUpdate string   `json:"system_prompt_update,omitempty"`
	ToolUpdate         string   `json:"tool_update,omitempty"`
	MinHarnessVersion  string   `json:"min_harness_version,omitempty"`
	Supported          bool     `json:"supported"`
	Reason             string   `json:"reason,omitempty"`
}

// BuiltinModels is the pinned API-key route's advisory catalog. It is not a
// probe and the server may reject a listed model for a particular account/key.
func BuiltinModels() []ModelInfo {
	return []ModelInfo{{ID: "deepseek-flash", Name: "DeepSeek Flash", ContextWindow: 1000000, MaxTokens: 256000, InputModalities: []string{"text", "image"}, SystemPromptUpdate: "in-history", ToolUpdate: "addition-only", Supported: true}, {ID: "deepseek-v4-pro", Name: "DeepSeek V4 Pro", ContextWindow: 1000000, MaxTokens: 256000, InputModalities: []string{"text"}, Supported: true}}
}

// ListAccountModels reads GET /models at the configured inference origin, not
// /anthropic/v1/models. It preserves refused entries so a UI can explain them.
// The caller owns refresh/fallback policy and must filter Supported for menus.
func (c *Client) ListAccountModels(ctx context.Context) ([]ModelInfo, error) {
	if err := c.ready(); err != nil {
		return nil, err
	}
	if c.token == "" {
		return nil, &Error{Code: "ACCOUNT_SIGN_IN_REQUIRED"}
	}
	u, _ := url.Parse(c.root)
	u.Path = "/models"
	u.RawPath = ""
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, invalid()
	}
	request.Header.Set("x-dsh-auth-token", c.token)
	request.Header.Set("User-Agent", "dsgo/0.1 (DeepSeek account terminal client)")
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{Code: "TRANSPORT"}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, c.responseError(response)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20+1))
	if err != nil {
		return nil, &Error{Code: "TRANSPORT"}
	}
	if len(data) > 2<<20 {
		return nil, &Error{Code: "RESPONSE_LIMIT"}
	}
	return parseCatalog(data)
}

func parseCatalog(data []byte) ([]ModelInfo, error) {
	var envelope struct {
		Data []json.RawMessage `json:"data"`
	}
	if json.Unmarshal(data, &envelope) != nil || envelope.Data == nil {
		return nil, &Error{Code: "INVALID_RESPONSE"}
	}
	out := []ModelInfo{}
	seen := map[string]bool{}
	for _, raw := range envelope.Data {
		var entry struct {
			ID               string                     `json:"id"`
			Name             string                     `json:"name"`
			ContextWindow    json.RawMessage            `json:"context_window"`
			MaxOutputTokens  json.RawMessage            `json:"max_output_tokens"`
			InputModalities  json.RawMessage            `json:"input_modalities"`
			OutputModalities json.RawMessage            `json:"output_modalities"`
			Requirements     json.RawMessage            `json:"client_requirements"`
			Capabilities     map[string]json.RawMessage `json:"api_capabilities"`
		}
		if json.Unmarshal(raw, &entry) != nil || entry.ID == "" || seen[entry.ID] {
			continue
		}
		seen[entry.ID] = true
		info := ModelInfo{ID: entry.ID, Name: entry.Name, InputModalities: []string{"text"}, Supported: true}
		known := entry.ID == "deepseek-flash" || entry.ID == "deepseek-v4-pro"
		if entry.ID == "deepseek-flash" {
			info.ToolUpdate = "addition-only"
			info.SystemPromptUpdate = "in-history"
		}
		if len(entry.Requirements) > 0 {
			var requirements map[string]json.RawMessage
			if json.Unmarshal(entry.Requirements, &requirements) != nil || requirements == nil {
				info.Reason = "MODEL_UNSUPPORTED"
			} else if value, ok := requirements["deepseek-harness"]; ok {
				var requirement map[string]json.RawMessage
				if json.Unmarshal(value, &requirement) != nil || requirement == nil {
					info.Reason = "MODEL_UNSUPPORTED"
				} else if minimum, ok := requirement["min_version"]; ok {
					if json.Unmarshal(minimum, &info.MinHarnessVersion) != nil || info.MinHarnessVersion == "" {
						info.Reason = "MODEL_UNSUPPORTED"
					} else if cmp, ok := compareVersions(BaselineVersion, info.MinHarnessVersion); !ok {
						info.Reason = "MODEL_UNSUPPORTED"
					} else if cmp < 0 {
						info.Reason = "CLIENT_UPGRADE_REQUIRED"
					} else {
						known = true
					}
				}
			}
		}
		if !known && info.Reason == "" {
			info.Reason = "MODEL_UNSUPPORTED"
		}
		var messages map[string]json.RawMessage
		if value, ok := entry.Capabilities["anthropic_messages"]; !ok || json.Unmarshal(value, &messages) != nil || messages == nil {
			info.Reason = "MODEL_UNSUPPORTED"
		}
		if value, ok := messages["system_prompt_update"]; ok {
			var mode string
			if json.Unmarshal(value, &mode) == nil && mode == "in-history" {
				info.SystemPromptUpdate = mode
			} else {
				info.SystemPromptUpdate = ""
			}
		}
		if len(entry.InputModalities) > 0 {
			info.InputModalities = modalities(entry.InputModalities)
			if len(info.InputModalities) == 0 {
				info.Reason = "MODEL_UNSUPPORTED"
			}
		}
		if len(entry.OutputModalities) > 0 {
			outputs := modalities(entry.OutputModalities)
			found := false
			for _, m := range outputs {
				found = found || m == "text"
			}
			if !found {
				info.Reason = "MODEL_UNSUPPORTED"
			}
		}
		if len(entry.ContextWindow) > 0 {
			if json.Unmarshal(entry.ContextWindow, &info.ContextWindow) != nil || info.ContextWindow <= 0 {
				info.Reason = "MODEL_UNSUPPORTED"
			}
		}
		if len(entry.MaxOutputTokens) > 0 {
			if json.Unmarshal(entry.MaxOutputTokens, &info.MaxTokens) != nil || info.MaxTokens <= 0 {
				info.Reason = "MODEL_UNSUPPORTED"
			}
		} else {
			info.MaxTokens = 256000
		}
		info.MaxTokens = min(info.MaxTokens, 256000)
		if info.ContextWindow == 0 {
			info.ContextWindow = 1000000
		}
		info.Supported = info.Reason == ""
		out = append(out, info)
	}
	if len(out) == 0 {
		return nil, &Error{Code: "INVALID_RESPONSE"}
	}
	return out, nil
}

func modalities(data []byte) []string {
	var values []string
	if json.Unmarshal(data, &values) != nil {
		return nil
	}
	out := []string{}
	seen := map[string]bool{}
	for _, value := range values {
		if (value == "text" || value == "image") && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

type semanticVersion struct {
	numbers [3]int
	pre     []string
}

func versionOf(value string) (semanticVersion, bool) {
	var version semanticVersion
	value = strings.TrimPrefix(value, "v")
	main, build, _ := strings.Cut(value, "+")
	if build != "" {
		for _, part := range strings.Split(build, ".") {
			if !versionIdentifier(part) {
				return version, false
			}
		}
	}
	main, pre, hasPre := strings.Cut(main, "-")
	parts := strings.Split(main, ".")
	if len(parts) != 3 {
		return version, false
	}
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return version, false
		}
		for _, b := range part {
			if b < '0' || b > '9' {
				return version, false
			}
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return version, false
		}
		version.numbers[i] = n
	}
	if hasPre {
		version.pre = strings.Split(pre, ".")
		for _, part := range version.pre {
			if !versionIdentifier(part) {
				return version, false
			}
			if numeric(part) && len(part) > 1 && part[0] == '0' {
				return version, false
			}
		}
	}
	return version, true
}
func versionIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, b := range value {
		if !(b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b == '-') {
			return false
		}
	}
	return true
}
func numeric(value string) bool {
	for _, b := range value {
		if b < '0' || b > '9' {
			return false
		}
	}
	return value != ""
}
func compareVersions(left, right string) (int, bool) {
	a, ok := versionOf(left)
	if !ok {
		return 0, false
	}
	b, ok := versionOf(right)
	if !ok {
		return 0, false
	}
	for i := 0; i < 3; i++ {
		if a.numbers[i] < b.numbers[i] {
			return -1, true
		}
		if a.numbers[i] > b.numbers[i] {
			return 1, true
		}
	}
	if len(a.pre) == 0 && len(b.pre) > 0 {
		return 1, true
	}
	if len(b.pre) == 0 && len(a.pre) > 0 {
		return -1, true
	}
	for i := 0; i < min(len(a.pre), len(b.pre)); i++ {
		x, y := a.pre[i], b.pre[i]
		if x == y {
			continue
		}
		if numeric(x) && numeric(y) {
			if len(x) < len(y) {
				return -1, true
			}
			if len(x) > len(y) {
				return 1, true
			}
		} else if numeric(x) {
			return -1, true
		} else if numeric(y) {
			return 1, true
		}
		return strings.Compare(x, y), true
	}
	if len(a.pre) < len(b.pre) {
		return -1, true
	}
	if len(a.pre) > len(b.pre) {
		return 1, true
	}
	return 0, true
}
