// Package state stores dsgo configuration and append-only session events.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type MCPServer struct {
	Command  string            `json:"command,omitempty"`
	Args     []string          `json:"args,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	CWD      string            `json:"cwd,omitempty"`
	URL      string            `json:"url,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Disabled bool              `json:"disabled,omitempty"`
}

type Config struct {
	BaseURL                string                     `json:"base_url"`
	APIKey                 string                     `json:"api_key,omitempty"`
	Account                json.RawMessage            `json:"account,omitempty"`
	Model                  string                     `json:"model"`
	Thinking               string                     `json:"thinking"`
	MaxTokens              int                        `json:"max_tokens"`
	MaxSteps               int                        `json:"max_steps"`
	Permission             string                     `json:"permission"`
	ApprovalPolicy         string                     `json:"approval_policy"`
	ExecutionMode          string                     `json:"execution_mode"`
	AgentPreset            string                     `json:"agent_preset"`
	SubagentMaxDepth       *int                       `json:"subagent_max_depth,omitempty"`
	SubagentModelSelection json.RawMessage            `json:"subagent_model_selection,omitempty"`
	NodePath               string                     `json:"node_path"`
	Language               string                     `json:"language"`
	ContextTokens          int                        `json:"context_tokens"`
	MCPServers             map[string]MCPServer       `json:"mcp_servers,omitempty"`
	ProviderProfiles       map[string]json.RawMessage `json:"provider_profiles,omitempty"`
	SelectedProvider       string                     `json:"selected_provider"`
}

func DefaultRoot() string {
	if v := os.Getenv("DSGO_HOME"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "dsgo")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".dsgo")
	}
	return filepath.Join(home, ".local", "share", "dsgo")
}

func DefaultConfig() Config {
	return Config{
		BaseURL: "https://api.deepseek.com/anthropic", Model: "deepseek-flash", Thinking: "high",
		MaxTokens: 16384, MaxSteps: 100, Permission: "workspace-write", ApprovalPolicy: "on-request",
		ExecutionMode: "native", AgentPreset: "standard", NodePath: "node", Language: "zh-CN", ContextTokens: 196608,
		MCPServers:       map[string]MCPServer{},
		ProviderProfiles: map[string]json.RawMessage{}, SelectedProvider: "deepseek-official",
	}
}

// LoadConfig reads persisted settings. Environment credentials are intentionally
// applied by the caller, so a later unrelated config edit never persists them.
func LoadConfig(root string) (Config, error) {
	c := DefaultConfig()
	if root == "" {
		root = DefaultRoot()
	}
	p := filepath.Join(root, "config.json")
	if err := rejectSymlink(p); err != nil {
		return c, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("decode config: %w", err)
	}
	if c.MCPServers == nil {
		c.MCPServers = map[string]MCPServer{}
	}
	if c.ProviderProfiles == nil {
		c.ProviderProfiles = map[string]json.RawMessage{}
	}
	return c, c.Validate()
}

// WithEnvironment returns runtime settings without changing the persisted file.
func (c Config) WithEnvironment() Config {
	if key := os.Getenv("DEEPSEEK_API_KEY"); key != "" {
		c.APIKey = key
	}
	if key := os.Getenv("DSGO_API_KEY"); key != "" {
		c.APIKey = key
	}
	if base := os.Getenv("DSGO_BASE_URL"); base != "" {
		c.BaseURL = base
	}
	if model := os.Getenv("DSGO_MODEL"); model != "" {
		c.Model = model
	}
	return c
}

func (c Config) Validate() error {
	if c.SubagentMaxDepth != nil && *c.SubagentMaxDepth < 0 {
		return errors.New("subagent_max_depth must be a non-negative integer")
	}
	if len(c.SubagentModelSelection) > 0 && !json.Valid(c.SubagentModelSelection) {
		return errors.New("subagent_model_selection must be JSON")
	}
	if len(c.Account) > 0 && !json.Valid(c.Account) {
		return errors.New("account must be valid JSON")
	}
	if strings.TrimSpace(c.SelectedProvider) == "" {
		return errors.New("selected_provider must not be empty")
	}
	for name, profile := range c.ProviderProfiles {
		if strings.TrimSpace(name) == "" || !json.Valid(profile) {
			return errors.New("provider_profiles must contain named valid JSON profiles")
		}
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("base_url must be an http(s) URL without credentials, query, or fragment")
	}
	if strings.TrimSpace(c.Model) == "" {
		return errors.New("model must not be empty")
	}
	if !oneOf(c.Thinking, "off", "low", "high", "max") && !((c.SelectedProvider != "" && c.SelectedProvider != "deepseek-official") && oneOf(c.Thinking, "minimal", "medium", "xhigh")) {
		return errors.New("thinking must be off, low, high, or max; configured providers may also use minimal, medium, xhigh")
	}
	if c.MaxTokens < 1 || c.MaxTokens > 1048576 {
		return errors.New("max_tokens must be between 1 and 1048576")
	}
	if c.MaxSteps < 1 || c.MaxSteps > 10000 {
		return errors.New("max_steps must be between 1 and 10000")
	}
	if c.ContextTokens < 1024 {
		return errors.New("context_tokens must be at least 1024")
	}
	if !oneOf(c.Permission, "read-only", "workspace-write", "full-access") {
		return errors.New("permission must be read-only, workspace-write, or full-access")
	}
	if !oneOf(c.ApprovalPolicy, "on-request", "never") {
		return errors.New("approval_policy must be on-request or never")
	}
	if !oneOf(c.ExecutionMode, "native", "ptc", "ptc-repl") {
		return errors.New("execution_mode must be native, ptc, or ptc-repl")
	}
	if !oneOf(c.AgentPreset, "standard", "ptc", "minimal", "cordis") {
		return errors.New("agent_preset must be standard, ptc, minimal, or cordis")
	}
	if strings.TrimSpace(c.NodePath) == "" {
		return errors.New("node_path must not be empty")
	}
	for name, s := range c.MCPServers {
		if len(name) < 1 || len(name) > 32 || strings.IndexFunc(name, func(r rune) bool {
			return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-')
		}) >= 0 {
			return errors.New("MCP server name must match [A-Za-z0-9_-]{1,32}")
		}
		if !s.Disabled && (s.Command == "") == (s.URL == "") {
			return fmt.Errorf("MCP server %q requires exactly one of command and url", name)
		}
	}
	return nil
}

func (c Config) Masked() Config {
	if len(c.Account) > 0 && string(c.Account) != "null" {
		c.Account = json.RawMessage(`{"authenticated":true}`)
	}
	profiles := make(map[string]json.RawMessage, len(c.ProviderProfiles))
	for name := range c.ProviderProfiles {
		profiles[name] = json.RawMessage(`{"configured":true}`)
	}
	c.ProviderProfiles = profiles
	if c.APIKey != "" {
		c.APIKey = "********"
	}
	servers := make(map[string]MCPServer, len(c.MCPServers))
	for name, s := range c.MCPServers {
		if parsed, err := url.Parse(s.URL); err == nil && s.URL != "" {
			if parsed.User != nil {
				parsed.User = url.User("********")
			}
			if parsed.RawQuery != "" {
				parsed.RawQuery = "redacted"
			}
			s.URL = parsed.String()
		}
		env := make(map[string]string, len(s.Env))
		for k := range s.Env {
			env[k] = "********"
		}
		s.Env = env
		headers := make(map[string]string, len(s.Headers))
		for k := range s.Headers {
			headers[k] = "********"
		}
		s.Headers = headers
		servers[name] = s
	}
	c.MCPServers = servers
	return c
}

func SaveConfig(root string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	s, err := Open(root)
	if err != nil {
		return err
	}
	return s.withLock(func() error {
		b, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			return err
		}
		return atomicWrite(filepath.Join(s.root, "config.json"), append(b, '\n'))
	})
}

func oneOf(value string, choices ...string) bool {
	for _, c := range choices {
		if value == c {
			return true
		}
	}
	return false
}
