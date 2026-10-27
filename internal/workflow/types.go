// Package workflow ports the pinned official workflow-ptc script contract.
// Copyright (c) 2026 dsgo contributors and DeepSeek. MIT license.
package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

type Phase struct {
	Title    string  `json:"title"`
	Detail   *string `json:"detail,omitempty"`
	Provider *string `json:"provider,omitempty"`
	Model    *string `json:"model,omitempty"`
}
type Meta struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	WhenToUse   *string `json:"whenToUse,omitempty"`
	Phases      []Phase `json:"phases,omitempty"`
}
type Request struct {
	ParentID, CWD, WorkspaceRoot, Permission string
	Script                                   string `json:"script"`
	Meta                                     Meta   `json:"meta"`
	Args                                     any    `json:"args,omitempty"`
	Observe                                  func(Event)
}
type ChildRequest struct {
	Seq      int             `json:"seq"`
	Prompt   string          `json:"prompt"`
	Label    string          `json:"label"`
	Phase    *string         `json:"phase,omitempty"`
	Provider string          `json:"provider,omitempty"`
	Model    string          `json:"model,omitempty"`
	Schema   json.RawMessage `json:"schema,omitempty"`
}
type ChildResult struct {
	Text       string `json:"text,omitempty"`
	Structured any    `json:"structured,omitempty"`
	StopReason string `json:"stopReason"`
}
type Result struct {
	Value         any    `json:"value"`
	StopReason    string `json:"stopReason"`
	Error         string `json:"error,omitempty"`
	AgentsStarted int    `json:"agentsStarted"`
}
type Event struct {
	Type       string  `json:"type"`
	RunID      string  `json:"runId"`
	Seq        int     `json:"seq,omitempty"`
	Name       string  `json:"name,omitempty"`
	Label      string  `json:"label,omitempty"`
	Phase      *string `json:"phase,omitempty"`
	ChildID    string  `json:"childId,omitempty"`
	Outcome    string  `json:"outcome,omitempty"`
	StopReason string  `json:"stopReason,omitempty"`
	Message    string  `json:"message,omitempty"`
}
type Command func(context.Context, string, string, string, string, ...string) (*exec.Cmd, func(), error)
type Child func(context.Context, string, ChildRequest, func(string)) (ChildResult, error)
type Options struct {
	NodePath                                                            string
	Command                                                             Command
	Child                                                               Child
	MaxConcurrentAgents, MaxTotalAgents, MaxItemsPerCall, SyncTimeoutMS int
}

func (o *Options) defaults() {
	if o.NodePath == "" {
		o.NodePath = "node"
	}
	if o.MaxConcurrentAgents <= 0 {
		o.MaxConcurrentAgents = min(16, max(1, runtime.NumCPU()-2))
	}
	if o.MaxTotalAgents <= 0 {
		o.MaxTotalAgents = 1000
	}
	if o.MaxItemsPerCall <= 0 {
		o.MaxItemsPerCall = 4096
	}
	if o.SyncTimeoutMS <= 0 {
		o.SyncTimeoutMS = 5000
	}
}
func Decode(raw json.RawMessage) (Request, bool, error) {
	var wire struct {
		Script     string          `json:"script"`
		Meta       json.RawMessage `json:"meta"`
		Args       json.RawMessage `json:"args"`
		Background bool            `json:"run_in_background"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(&wire); e != nil {
		return Request{}, false, e
	}
	m, e := ValidateMeta(wire.Meta)
	if e != nil {
		return Request{}, false, e
	}
	if wire.Script == "" {
		return Request{}, false, errors.New("script is required")
	}
	var args any
	if len(wire.Args) > 0 {
		if e = json.Unmarshal(wire.Args, &args); e != nil {
			return Request{}, false, e
		}
		if _, ok := args.(map[string]any); !ok {
			return Request{}, false, errors.New("args must be an object")
		}
	}
	return Request{Script: wire.Script, Meta: m, Args: args}, wire.Background, nil
}
func ValidateMeta(raw json.RawMessage) (Meta, error) {
	var shape map[string]json.RawMessage
	if e := json.Unmarshal(raw, &shape); e != nil || shape == nil {
		return Meta{}, errors.New("META_INVALID: meta must be an object")
	}
	var errs []string
	for k := range shape {
		if k != "name" && k != "description" && k != "whenToUse" && k != "phases" {
			errs = append(errs, "meta."+k+" is not a recognized field")
		}
	}
	var m Meta
	for _, v := range []struct {
		n string
		p *string
	}{{"name", &m.Name}, {"description", &m.Description}} {
		if e := json.Unmarshal(shape[v.n], v.p); e != nil || *v.p == "" {
			errs = append(errs, "meta."+v.n+" must be a non-empty string")
		}
	}
	if v, ok := shape["whenToUse"]; ok {
		var x string
		if e := json.Unmarshal(v, &x); e != nil || string(v) == "null" {
			errs = append(errs, "meta.whenToUse must be a string")
		} else {
			m.WhenToUse = &x
		}
	}
	if v, ok := shape["phases"]; ok {
		var phases []map[string]json.RawMessage
		if e := json.Unmarshal(v, &phases); e != nil || string(v) == "null" {
			errs = append(errs, "meta.phases must be an array")
		} else {
			for i, p := range phases {
				prefix := fmt.Sprintf("meta.phases[%d]", i)
				var phase Phase
				for k := range p {
					if k != "title" && k != "detail" && k != "provider" && k != "model" {
						errs = append(errs, prefix+"."+k+" is not a recognized field")
					}
				}
				if e := json.Unmarshal(p["title"], &phase.Title); e != nil || phase.Title == "" {
					errs = append(errs, prefix+".title must be a non-empty string")
				}
				for _, f := range []struct {
					n string
					p **string
				}{{"detail", &phase.Detail}, {"provider", &phase.Provider}, {"model", &phase.Model}} {
					if v, ok := p[f.n]; ok {
						var s string
						if e := json.Unmarshal(v, &s); e != nil || string(v) == "null" {
							errs = append(errs, prefix+"."+f.n+" must be a string")
						} else {
							*f.p = &s
						}
					}
				}
				m.Phases = append(m.Phases, phase)
			}
		}
	}
	if len(errs) > 0 {
		return Meta{}, errors.New("META_INVALID: " + strings.Join(errs, "; "))
	}
	return m, nil
}

const Guidance = "\nUse the workflow tool only when the user explicitly asks for a workflow or large multi-agent orchestration. Prefer ordinary subagent calls for one or two delegations.\n"
