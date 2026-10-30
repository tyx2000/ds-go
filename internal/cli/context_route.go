package cli

import (
	"context"

	"dsgo/internal/agent"
	"dsgo/internal/attachments"
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
func (a *App) contextRoute(ctx context.Context, req core.Request) (agent.ContextRoute, error) {
	route := agent.ContextRoute{ContextWindow: a.cfg.ContextTokens, FileText: attachments.FileHandleText}
	if a.requestProvider(req) != "deepseek-official" {
		return route, nil
	}
	info, err := a.deepSeekModel(ctx, req)
	if err != nil {
		return route, err
	}
	route.ContextWindow, route.DefaultMaxTokens = info.ContextWindow, info.MaxTokens
	if route.ContextWindow <= 0 {
		route.ContextWindow = officialContextWindow
	}
	route.ImagePricing = a.priceRequestImages(ctx, req)
	return route, nil
}

func (a *App) priceRequestImages(ctx context.Context, req core.Request) func([]core.Block) ([]agent.ContextImagePrice, error) {
	return func(blocks []core.Block) ([]agent.ContextImagePrice, error) {
		prices := make([]agent.ContextImagePrice, 0, len(blocks))
		for _, block := range blocks {
			ref, durable, err := a.attachmentStore.ImageReference(ctx, block)
			if err != nil {
				return nil, err
			}
			plan, err := a.projectRequestImage(ctx, req, ref, durable)
			if err != nil {
				return nil, err
			}
			price := agent.ContextImagePrice{Text: plan.Text}
			if !plan.Omit {
				w, h := ref.Width, ref.Height
				if plan.Target != nil {
					w, h = plan.Target.Width, plan.Target.Height
				}
				price.VisualTokens, err = model.DeepSeekImageTokens(w, h)
				if err != nil {
					return nil, err
				}
			}
			prices = append(prices, price)
		}
		return prices, nil
	}
}
