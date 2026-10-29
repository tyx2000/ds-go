package model

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"dsgo/internal/core"
	"dsgo/internal/state"
)

const signedReplayFixture = `{"id":"m1","type":"message","content":[{"type":"thinking","thinking":"visible thought","signature":"opaque-signature"},{"type":"redacted_thinking","data":"opaque-redacted"},{"type":"text","text":"visible answer"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`
const responsesReplayFixture = `{"id":"r1","status":"completed","output":[{"id":"reason-1","type":"reasoning","encrypted_content":"opaque-encrypted","summary":[{"type":"summary_text","text":"visible thought"}]},{"id":"message-1","type":"message","role":"assistant","content":[{"type":"output_text","text":"visible answer"}]}]}`

func replayFixtureTransport(t *testing.T, api string, bodies *[]string) *http.Client {
	t.Helper()
	return &http.Client{Transport: providerRoundTrip(func(r *http.Request) (*http.Response, error) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		*bodies = append(*bodies, string(data))
		fixture := signedReplayFixture
		if api == "openai-responses" {
			fixture = responsesReplayFixture
		}
		return providerReply("application/json", fixture), nil
	})}
}

func replayMessage(response core.Response, request core.Request) core.Message {
	return core.Message{Role: "assistant", Content: response.Content, ReplayProvider: request.Provider, ReplayModel: request.Model, ReplayIdentity: response.ReplayIdentity}
}

func TestProviderOpaqueReplayRequiresEffectiveRouteIdentity(t *testing.T) {
	for _, api := range []string{"anthropic-messages", "openai-responses"} {
		t.Run(api, func(t *testing.T) {
			var initialBodies []string
			base := ProviderOptions{API: api, BaseURL: "https://gateway.invalid/a/v1", APIKey: "credential-alpha", Headers: map[string]string{"x-tenant-secret": "tenant-alpha"}, ReplayKey: bytes.Repeat([]byte{7}, 32), HTTPClient: replayFixtureTransport(t, api, &initialBodies)}
			initial, err := NewProvider(base)
			if err != nil {
				t.Fatal(err)
			}
			input := providerRequest()
			first, err := initial.Stream(context.Background(), input, nil)
			if err != nil || first.ReplayIdentity == "" {
				t.Fatalf("initial replay identity: %v %+v", err, first)
			}
			for _, test := range []struct {
				name   string
				change func(*ProviderOptions, *core.Request)
				keep   bool
			}{
				{"same-route-new-client", func(*ProviderOptions, *core.Request) {}, true},
				{"endpoint-host", func(o *ProviderOptions, _ *core.Request) { o.BaseURL = "https://other.invalid/a/v1" }, false},
				{"endpoint-path", func(o *ProviderOptions, _ *core.Request) { o.BaseURL = "https://gateway.invalid/b/v1" }, false},
				{"credential", func(o *ProviderOptions, _ *core.Request) { o.APIKey = "credential-beta" }, false},
				{"secret-header", func(o *ProviderOptions, _ *core.Request) {
					o.Headers = map[string]string{"X-Tenant-Secret": "tenant-beta"}
				}, false},
				{"header-case", func(o *ProviderOptions, _ *core.Request) {
					o.Headers = map[string]string{"X-TENANT-SECRET": "tenant-alpha"}
				}, true},
				{"private-key", func(o *ProviderOptions, _ *core.Request) { o.ReplayKey = bytes.Repeat([]byte{8}, 32) }, false},
				{"legacy-no-identity", func(_ *ProviderOptions, r *core.Request) { r.Messages[1].ReplayIdentity = "" }, false},
				{"different-provider", func(_ *ProviderOptions, r *core.Request) { r.Provider = "rebound-route" }, false},
				{"different-model", func(_ *ProviderOptions, r *core.Request) { r.Model = "another-model" }, false},
				{"forged-request-identity", func(o *ProviderOptions, r *core.Request) {
					o.BaseURL = "https://other.invalid/v1"
					r.ReplayIdentity = first.ReplayIdentity
				}, false},
			} {
				t.Run(test.name, func(t *testing.T) {
					opts := base
					request := input
					request.Messages = []core.Message{core.TextMessage("user", "initial"), replayMessage(first, input), core.TextMessage("user", "next")}
					test.change(&opts, &request)
					var bodies []string
					opts.HTTPClient = replayFixtureTransport(t, api, &bodies)
					client, err := NewProvider(opts)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = client.Stream(context.Background(), request, nil); err != nil {
						t.Fatal(err)
					}
					body := bodies[0]
					if strings.Contains(body, "opaque-") != test.keep || !strings.Contains(body, "visible thought") || !strings.Contains(body, "visible answer") {
						t.Fatalf("keep=%v projected %s", test.keep, body)
					}
					for _, secret := range []string{first.ReplayIdentity, "credential-alpha", "tenant-alpha", "replay_identity", "replay_provider"} {
						if strings.Contains(body, secret) {
							t.Fatalf("private provenance in model payload: %s", secret)
						}
					}
				})
			}
		})
	}
}

func TestProviderReplayIdentityTracksCurrentEnvironmentCredential(t *testing.T) {
	const keyEnv = "DSGO_REPLAY_FIXTURE_KEY"
	t.Setenv(keyEnv, "first-credential")
	var bodies []string
	client, err := NewProvider(ProviderOptions{API: "openai-responses", APIKeyEnv: keyEnv, ReplayKey: bytes.Repeat([]byte{1}, 32), HTTPClient: replayFixtureTransport(t, "openai-responses", &bodies)})
	if err != nil {
		t.Fatal(err)
	}
	input := providerRequest()
	first, err := client.Stream(context.Background(), input, nil)
	if err != nil {
		t.Fatal(err)
	}
	input.Messages = append(input.Messages, replayMessage(first, input), core.TextMessage("user", "next"))
	t.Setenv(keyEnv, "second-credential")
	second, err := client.Stream(context.Background(), input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ReplayIdentity == second.ReplayIdentity || strings.Contains(bodies[1], "opaque-encrypted") {
		t.Fatalf("rotated credential reused replay: %s", bodies[1])
	}
}

func TestDirectReplayIdentityScopesEndpointAndAuthenticationMode(t *testing.T) {
	key := bytes.Repeat([]byte{2}, 32)
	var bodies []string
	opts := Options{BaseURL: "https://gateway.invalid/anthropic", APIKey: "same-credential", ReplayKey: key, HTTPClient: replayFixtureTransport(t, "anthropic-messages", &bodies)}
	initial := New(opts)
	input := request()
	input.Provider = "deepseek-official"
	first, err := initial.Stream(context.Background(), input, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*Options)
		keep   bool
	}{
		{"same-effective-v1-root", func(o *Options) { o.BaseURL += "/v1/" }, true},
		{"different-credential", func(o *Options) { o.APIKey = "new-credential" }, false},
		{"same-value-different-auth-mode", func(o *Options) { o.APIKey = ""; o.Token = "same-credential" }, false},
		{"different-endpoint", func(o *Options) { o.BaseURL = "https://gateway.invalid/another" }, false},
		{"no-persistent-key", func(o *Options) { o.ReplayKey = nil }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := opts
			test.change(&changed)
			var captured []string
			changed.HTTPClient = replayFixtureTransport(t, "anthropic-messages", &captured)
			r := input
			r.Messages = append([]core.Message{core.TextMessage("user", "initial")}, replayMessage(first, input), core.TextMessage("user", "next"))
			if _, err := New(changed).Stream(context.Background(), r, nil); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(captured[0], "opaque-signature") != test.keep || strings.Contains(captured[0], "opaque-redacted") != test.keep {
				t.Fatalf("keep=%v body=%s", test.keep, captured[0])
			}
		})
	}
}

func TestForkKeepsProvenanceButReboundRouteOmitsOpaqueState(t *testing.T) {
	var bodies []string
	opts := ProviderOptions{API: "openai-responses", BaseURL: "https://gateway.invalid/v1", APIKey: "old-credential", ReplayKey: bytes.Repeat([]byte{4}, 32), HTTPClient: replayFixtureTransport(t, "openai-responses", &bodies)}
	client, err := NewProvider(opts)
	if err != nil {
		t.Fatal(err)
	}
	input := providerRequest()
	first, err := client.Stream(context.Background(), input, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.Create(t.TempDir(), "parent")
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []core.Message{core.TextMessage("user", "initial"), replayMessage(first, input), core.TextMessage("user", "next")} {
		kind := message.Role + "/message"
		if _, err = store.Append(parent.ID, "turn", kind, message); err != nil {
			t.Fatal(err)
		}
	}
	child, err := store.Fork(parent.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	input.Messages, err = store.Messages(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Messages) != 3 || input.Messages[1].ReplayIdentity != first.ReplayIdentity {
		t.Fatalf("fork provenance lost: %+v", input.Messages)
	}
	opts.APIKey = "new-credential"
	client, err = NewProvider(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Stream(context.Background(), input, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bodies[len(bodies)-1], "opaque-encrypted") {
		t.Fatal("fork leaked old encrypted state to rebound route")
	}
	encoded, _ := json.Marshal(input.Messages)
	if !strings.Contains(string(encoded), "opaque-encrypted") {
		t.Fatal("projection mutated durable history")
	}
}
