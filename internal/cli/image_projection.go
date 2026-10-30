package cli

import (
	"context"
	"crypto/sha256"
	"sync"
	"time"

	"dsgo/internal/attachments"
	"dsgo/internal/core"
	"dsgo/internal/model"
)

type catalogFlight struct {
	done   chan struct{}
	models []model.ModelInfo
	err    error
}
type deepSeekCatalogCache struct {
	mu      sync.Mutex
	entries map[[32]byte]*catalogFlight
}

// The account catalog is a connection snapshot shared by pricing and request
// projection. A cancelled meter waiter cannot cancel another live request.
// Failed fetches are evicted so a subsequent request can recover.
func (a *App) deepSeekModels(ctx context.Context) ([]model.ModelInfo, error) {
	key, token, err := credentials(a.cfg)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return model.BuiltinModels(), nil
	}
	base := a.cfg.BaseURL
	identity := sha256.Sum256(core.JSON([]string{base, key, token}))
	cache := &a.pricingCatalogs
	cache.mu.Lock()
	if cache.entries == nil {
		cache.entries = make(map[[32]byte]*catalogFlight)
	}
	flight := cache.entries[identity]
	if flight == nil {
		flight = &catalogFlight{done: make(chan struct{})}
		cache.entries[identity] = flight
		parent := a.ctx
		if parent == nil {
			parent = context.Background()
		}
		go func() {
			fetchCtx, cancel := context.WithTimeout(parent, 15*time.Second)
			defer cancel()
			models, err := model.New(model.Options{BaseURL: base, APIKey: key, Token: token}).ListAccountModels(fetchCtx)
			cache.mu.Lock()
			flight.models, flight.err = models, err
			if err != nil {
				delete(cache.entries, identity)
			}
			close(flight.done)
			cache.mu.Unlock()
		}()
	}
	cache.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-flight.done:
		return append([]model.ModelInfo(nil), flight.models...), flight.err
	}
}

func (a *App) requestProvider(req core.Request) string {
	if req.Provider != "" {
		return req.Provider
	}
	if a.cfg.SelectedProvider != "" {
		return a.cfg.SelectedProvider
	}
	return "deepseek-official"
}

func (a *App) deepSeekModel(ctx context.Context, req core.Request) (model.ModelInfo, error) {
	catalog, err := a.deepSeekModels(ctx)
	if err != nil {
		return model.ModelInfo{}, err
	}
	for _, info := range catalog {
		if info.ID == req.Model {
			return info, nil
		}
	}
	return model.ModelInfo{ID: req.Model, ContextWindow: officialContextWindow}, nil
}

func (a *App) projectRequestImage(ctx context.Context, req core.Request, ref attachments.ImageRef, durable bool) (attachments.ImageProjection, error) {
	// The pinned pi-ai adapter has no provider image-price override. Preserve
	// that upstream fallback instead of applying DeepSeek prices to other APIs.
	if a.requestProvider(req) != "deepseek-official" {
		return attachments.ImageProjection{}, nil
	}
	info, err := a.deepSeekModel(ctx, req)
	if err != nil {
		return attachments.ImageProjection{}, err
	}
	vision := false
	for _, modality := range info.InputModalities {
		vision = vision || modality == "image"
	}
	if !vision {
		return attachments.ImageProjection{Omit: true, Text: attachments.TextOnlyImageHandle(ref)}, nil
	}
	// Old inline blocks lack a durable attachment identity. Preserve their
	// bytes and price their actual dimensions rather than inventing a handle.
	if !durable {
		return attachments.ImageProjection{}, nil
	}
	w, h, err := model.DeepSeekRequestImageDimensions(ref.Width, ref.Height)
	if err != nil {
		return attachments.ImageProjection{}, err
	}
	access := ""
	if a.attachmentStore != nil {
		access, err = a.attachmentStore.ImageHostPath(ref)
		if err != nil {
			return attachments.ImageProjection{}, err
		}
	}
	return attachments.ImageProjection{
		Target: &attachments.RequestImageTarget{Width: w, Height: h, MaxBytes: 2 * 1024 * 1024},
		Text:   attachments.RequestImageHandle(ref, w, h, access),
	}, nil
}
