// Package core defines the wire values shared by dsgo's fixed built-in modules.
package core

import (
	"context"
	"encoding/json"
	"time"
)

const UpstreamCommit = "687f282e8625c6b8d11ff58211a90524be2e4317"

type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   any             `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Source    any             `json:"source,omitempty"`
	ToolName  string          `json:"tool_name,omitempty"`
}

type Message struct {
	Source         any     `json:"source,omitempty"`
	DeliveryID     string  `json:"delivery_id,omitempty"`
	Role           string  `json:"role"`
	Content        []Block `json:"content"`
	ReplayModel    string  `json:"replay_model,omitempty"`
	ReplayProvider string  `json:"replay_provider,omitempty"`
	ReplayIdentity string  `json:"replay_identity,omitempty"`
}

type ToolSpec struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	DeferLoading bool            `json:"defer_loading,omitempty"`
}

type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_input_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_creation_input_tokens,omitempty"`
}

type Request struct {
	Provider           string     `json:"-"`
	ReplayIdentity     string     `json:"-"` // adapter-owned provenance, never sent to the provider
	Model              string     `json:"model"`
	System             string     `json:"-"`
	Messages           []Message  `json:"messages"`
	Tools              []ToolSpec `json:"tools,omitempty"`
	MaxTokens          int        `json:"max_tokens"`
	Thinking           string     `json:"-"`
	Temperature        *float64   `json:"temperature,omitempty"`
	StopSequences      []string   `json:"stop_sequences,omitempty"`
	SessionID          string     `json:"-"`
	UserID             string     `json:"-"`
	Purpose            string     `json:"-"`
	SystemPromptUpdate string     `json:"-"`
	ToolUpdate         string     `json:"-"`
}

type Response struct {
	ReplayIdentity string  `json:"replay_identity,omitempty"`
	ID             string  `json:"id"`
	Content        []Block `json:"content"`
	StopReason     string  `json:"stop_reason"`
	Usage          Usage   `json:"usage"`
}

type Delta struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	Index int    `json:"index,omitempty"`
}

type Model interface {
	Stream(context.Context, Request, func(Delta)) (Response, error)
}

type Event struct {
	Seq    int64           `json:"seq"`
	ID     string          `json:"id"`
	Time   time.Time       `json:"time"`
	Type   string          `json:"type"`
	TurnID string          `json:"turn_id,omitempty"`
	Data   json.RawMessage `json:"data"`
}

type Session struct {
	CreationLabel string    `json:"creation_label,omitempty"`
	Delegated     bool      `json:"delegated,omitempty"`
	Plan          bool      `json:"plan,omitempty"`
	Provider      string    `json:"provider,omitempty"`
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	CWD           string    `json:"cwd"`
	ParentID      string    `json:"parent_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	Archived      bool      `json:"archived,omitempty"`
	Model         string    `json:"model,omitempty"`
	Thinking      string    `json:"thinking,omitempty"`
	Permission    string    `json:"permission,omitempty"`
}

type ToolResult struct {
	Value   json.RawMessage `json:"-"` // execution-local canonical value; never persist inline media payloads
	Content string          `json:"content"`
	Blocks  []Block         `json:"blocks,omitempty"`
	Meta    any             `json:"meta,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
}

type Approval struct {
	ID          string          `json:"id"`
	SessionID   string          `json:"session_id"`
	Tool        string          `json:"tool"`
	Description string          `json:"description"`
	Input       json.RawMessage `json:"input"`
}

type Approver func(context.Context, Approval) (bool, error)

type ToolContext struct {
	SessionID      string
	TurnID         string
	CallID         string
	ExecutionID    string // unique top-level execution, inherited by nested dispatch
	CWD            string
	HarnessHome    string // trusted process data root for fresh-shell DSH_HOME
	ProfileName    string // launcher profile, absent when no launcher owns one
	ProfileDir     string
	WorkspaceRoot  string // stable writable project root; changing CWD never expands this boundary
	Permission     string // read-only, workspace-write, full-access
	ApprovalPolicy string // on-request, never
	Approve        Approver
	Emit           func(string, any) error
	Inject         func(Message) error // enqueue an attributed message after the current tool-results batch
}

type Executor interface {
	Specs() []ToolSpec
	Execute(context.Context, ToolContext, string, json.RawMessage) (ToolResult, error)
}

func JSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
func TextMessage(role, text string) Message {
	return Message{Role: role, Content: []Block{{Type: "text", Text: text}}}
}
