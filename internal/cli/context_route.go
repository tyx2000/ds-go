package cli

import (
	"context"

	"dsgo/internal/agent"
	"dsgo/internal/core"
	"dsgo/internal/model"
)

// officialContextWindow is the pinned llm-deepseek DEFAULT_CONTEXT_WINDOW,
// used for a DeepSeek model the built-in catalog does not list.
const officialContextWindow = 1000000

// contextRoute gives the token meter and automatic compaction the routed
// model's capacity: the DeepSeek catalog on the official route, and the
// context_tokens setting for configured providers, whose windows are only
// known from a network model listing.
func (a *App) contextRoute(_ context.Context, req core.Request) (agent.ContextRoute, error) {
	if req.Provider != "" && req.Provider != "deepseek-official" {
		return agent.ContextRoute{ContextWindow: a.cfg.ContextTokens}, nil
	}
	route := agent.ContextRoute{ContextWindow: officialContextWindow}
	for _, info := range model.BuiltinModels() {
		if info.ID == req.Model {
			route.ContextWindow, route.DefaultMaxTokens = info.ContextWindow, info.MaxTokens
		}
	}
	return route, nil
}
