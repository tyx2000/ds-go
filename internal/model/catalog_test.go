package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAccountModelListingAndVersionGates(t *testing.T) {
	rows := []any{
		map[string]any{"id": "deepseek-flash", "name": "Flash", "input_modalities": []string{"image", "text"}, "context_window": 1000000, "max_output_tokens": 32000, "api_capabilities": map[string]any{"anthropic_messages": map[string]any{"system_prompt_update": "in-history"}}},
		map[string]any{"id": "future", "client_requirements": map[string]any{"deepseek-harness": map[string]string{"min_version": "0.2.1"}}, "api_capabilities": map[string]any{"anthropic_messages": map[string]any{}}},
		map[string]any{"id": "new-supported", "client_requirements": map[string]any{"deepseek-harness": map[string]string{"min_version": "0.2.1-alpha.0"}}, "api_capabilities": map[string]any{"anthropic_messages": map[string]any{}}},
		map[string]any{"id": "unknown", "api_capabilities": map[string]any{"anthropic_messages": map[string]any{}}},
		map[string]any{"id": "deepseek-flash"}, nil,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" || r.Header.Get("x-dsh-auth-token") != "grant" {
			t.Error("wrong account listing request")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": rows})
	}))
	defer server.Close()
	models, err := New(Options{BaseURL: server.URL + "/anthropic", Token: "grant"}).ListAccountModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 4 || !models[0].Supported || models[0].MaxTokens != 32000 || models[0].InputModalities[0] != "image" || models[0].SystemPromptUpdate != "in-history" {
		t.Fatalf("catalog %#v", models)
	}
	if models[1].Supported || models[1].Reason != "CLIENT_UPGRADE_REQUIRED" || !models[2].Supported || models[3].Supported {
		t.Fatalf("gates %#v", models)
	}
	_, err = New(Options{BaseURL: server.URL, APIKey: "key"}).ListAccountModels(context.Background())
	assertCode(t, err, "ACCOUNT_SIGN_IN_REQUIRED")
}

func TestSemanticVersionOrdering(t *testing.T) {
	ordered := []string{"0.2.1-alpha.0", "0.2.1-alpha.2", "0.2.1-alpha.10", "0.2.1-beta", "0.2.1-rc.1", "0.2.1-rc.2", "0.2.1", "0.2.2"}
	for i := 0; i < len(ordered)-1; i++ {
		if cmp, ok := compareVersions(ordered[i], ordered[i+1]); !ok || cmp >= 0 {
			t.Fatalf("%s should precede %s", ordered[i], ordered[i+1])
		}
	}
	for _, value := range []string{"0.2.1-01", "0.2", "0.02.1", "0.2.1-", "not-a-version"} {
		if _, ok := versionOf(value); ok {
			t.Fatal(fmt.Sprint("accepted invalid version ", value))
		}
	}
}
