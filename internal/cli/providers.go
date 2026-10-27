package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"

	"dsgo/internal/core"
	"dsgo/internal/model"
	"dsgo/internal/state"
)

type modelRoutes struct {
	clients  map[string]core.Model
	fallback string
}

func (r modelRoutes) Stream(ctx context.Context, req core.Request, emit func(core.Delta)) (core.Response, error) {
	provider := req.Provider
	if provider == "" {
		provider = r.fallback
	}
	client, ok := r.clients[provider]
	if !ok {
		return core.Response{}, fmt.Errorf("provider %q is not configured", provider)
	}
	req.Provider = provider
	return client.Stream(ctx, req, emit)
}

func buildModels(cfg state.Config) (core.Model, error) {
	key, token, err := credentials(cfg)
	if err != nil {
		return nil, err
	}
	routes := modelRoutes{clients: map[string]core.Model{"deepseek-official": model.New(model.Options{BaseURL: cfg.BaseURL, APIKey: key, Token: token})}, fallback: cfg.SelectedProvider}
	if routes.fallback == "" {
		routes.fallback = "deepseek-official"
	}
	for name, raw := range cfg.ProviderProfiles {
		if name == "deepseek-official" {
			return nil, errors.New("provider profile name deepseek-official is reserved")
		}
		var options model.ProviderOptions
		if err = json.Unmarshal(raw, &options); err != nil {
			return nil, fmt.Errorf("provider %s: %w", name, err)
		}
		var extra struct {
			APIKeyEnv string `json:"api_key_env"`
		}
		if err = json.Unmarshal(raw, &extra); err != nil {
			return nil, err
		}
		if extra.APIKeyEnv != "" {
			if key := os.Getenv(extra.APIKeyEnv); key != "" {
				options.APIKey = key
			}
		}
		client, err := model.NewProvider(options)
		if err != nil {
			return nil, fmt.Errorf("provider %s: %w", name, err)
		}
		routes.clients[name] = client
	}
	if _, ok := routes.clients[routes.fallback]; !ok {
		return nil, fmt.Errorf("selected_provider %q is not configured", routes.fallback)
	}
	return routes, nil
}

func (a *App) modelsCommand(args []string) error {
	if len(args) > 0 && args[0] == "account" {
		key, token, err := credentials(a.cfg)
		if err != nil {
			return err
		}
		if token == "" {
			return errors.New("dsgo auth login is required for the account model catalog")
		}
		models, err := model.New(model.Options{BaseURL: a.cfg.BaseURL, APIKey: key, Token: token}).ListAccountModels(a.ctx)
		if err != nil {
			return err
		}
		a.print(models)
		return nil
	}
	if len(args) > 0 {
		raw, ok := a.cfg.ProviderProfiles[args[0]]
		if !ok {
			return fmt.Errorf("provider %q is not configured", args[0])
		}
		var opts model.ProviderOptions
		if err := json.Unmarshal(raw, &opts); err != nil {
			return err
		}
		client, err := model.NewProvider(opts)
		if err != nil {
			return err
		}
		models, err := client.ListModels(a.ctx)
		if err != nil {
			return err
		}
		a.print(models)
		return nil
	}
	a.print(map[string]any{"deepseek-official": model.BuiltinModels(), "configured_providers": a.cfg.Masked().ProviderProfiles, "selected_provider": a.cfg.SelectedProvider})
	return nil
}

func officialAPI(base string) bool {
	u, err := url.Parse(base)
	return err == nil && u.Scheme == "https" && u.Host == "api.deepseek.com"
}
