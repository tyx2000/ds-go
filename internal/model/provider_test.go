package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"dsgo/internal/core"
)

type providerRoundTrip func(*http.Request) (*http.Response, error)

func (f providerRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func providerReply(kind, body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{kind}}, Body: io.NopCloser(strings.NewReader(body))}
}
func chatFrame(value any) string { return "data: " + string(core.JSON(value)) + "\n\n" }
func chatJSON() string {
	return `{"id":"chat-1","choices":[{"index":0,"message":{"content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":2}}}`
}
func providerRequest() core.Request {
	return core.Request{Provider: "fixture-route", Model: "fixture-model", Thinking: "off", MaxTokens: 4096, Messages: []core.Message{core.TextMessage("user", "hello")}}
}

func TestProviderChatWireToolFragmentsAndUsage(t *testing.T) {
	t.Setenv("DSGO_FIXTURE_PROVIDER_KEY", "fixture-secret")
	stream := chatFrame(map[string]any{"id": "chat-1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": "reason", "tool_calls": []any{map[string]any{"index": 0, "id": "call-1", "type": "function", "function": map[string]string{"name": "read_file", "arguments": "{\"path\":"}}}}}}}) +
		chatFrame(map[string]any{"id": "chat-1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]string{"arguments": "\"a.txt\"}"}}}}, "finish_reason": "tool_calls"}}}) +
		chatFrame(map[string]any{"id": "chat-1", "choices": []any{}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 4, "prompt_tokens_details": map[string]int{"cached_tokens": 2}}}) + "data: [DONE]\n\n"
	client, err := NewProvider(ProviderOptions{API: "openai-completions", BaseURL: "https://gateway.invalid/custom", APIKeyEnv: "DSGO_FIXTURE_PROVIDER_KEY", ReasoningEfforts: map[string]string{"high": "xhigh"}, SystemRole: "developer", MaxTokensField: "max_tokens", HTTPClient: &http.Client{Transport: providerRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/custom/chat/completions" {
			t.Fatalf("base changed: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer fixture-secret" || r.Header.Get("x-api-key") != "" {
			t.Fatal("wrong authentication")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["max_tokens"] != float64(4096) || body["reasoning_effort"] != "xhigh" {
			t.Fatalf("wrong controls %#v", body)
		}
		messages := body["messages"].([]any)
		if messages[0].(map[string]any)["role"] != "developer" {
			t.Fatal("system role not mapped")
		}
		return providerReply("text/event-stream", stream), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	r := providerRequest()
	r.Thinking = "high"
	r.System = "system"
	r.Tools = []core.ToolSpec{{Name: "read_file", InputSchema: core.JSON(map[string]any{"type": "object"})}}
	response, err := client.Stream(context.Background(), r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.StopReason != "tool_use" || len(response.Content) != 2 || response.Content[1].ID != "call-1" || string(response.Content[1].Input) != `{"path":"a.txt"}` {
		t.Fatalf("bad response %#v", response)
	}
	if response.Usage.InputTokens != 8 || response.Usage.CacheReadTokens != 2 || response.Usage.OutputTokens != 4 {
		t.Fatalf("bad usage %#v", response.Usage)
	}
}

func TestProviderCredentialReferenceResolvesEachRequest(t *testing.T) {
	t.Setenv("DSGO_FIXTURE_PROVIDER_KEY", "first")
	calls := 0
	client, err := NewProvider(ProviderOptions{API: "openai-completions", APIKeyEnv: "DSGO_FIXTURE_PROVIDER_KEY", HTTPClient: &http.Client{Transport: providerRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		want := "Bearer first"
		if calls == 2 {
			want = "Bearer second"
		}
		if r.Header.Get("Authorization") != want {
			t.Fatal("stale credential")
		}
		return providerReply("application/json", chatJSON()), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Stream(context.Background(), providerRequest(), nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DSGO_FIXTURE_PROVIDER_KEY", "second")
	if _, err = client.Stream(context.Background(), providerRequest(), nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DSGO_FIXTURE_PROVIDER_KEY", "")
	_, err = client.Stream(context.Background(), providerRequest(), nil)
	assertCode(t, err, "MISSING_CREDENTIAL")
	if calls != 2 {
		t.Fatal("missing credential was dispatched")
	}
}

func TestProviderNoRetryAfterContentAndPreContentRetry(t *testing.T) {
	partial := chatFrame(map[string]any{"id": "chat-1", "choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": "partial"}}}})
	calls := 0
	client, _ := NewProvider(ProviderOptions{API: "openai-completions", APIKey: "fixture-secret", MaxRetries: 3, RetryDelay: time.Nanosecond, HTTPClient: &http.Client{Transport: providerRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		return providerReply("text/event-stream", partial), nil
	})}})
	response, err := client.Stream(context.Background(), providerRequest(), nil)
	assertCode(t, err, "STREAM_CLOSED")
	if calls != 1 || response.Content[0].Text != "partial" {
		t.Fatalf("replayed partial content: %d %#v", calls, response)
	}
	calls = 0
	client, _ = NewProvider(ProviderOptions{API: "openai-completions", APIKey: "fixture-secret", MaxRetries: 1, RetryDelay: time.Nanosecond, HTTPClient: &http.Client{Transport: providerRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			reply := providerReply("application/json", `{"error":{"message":"fixture-secret"}}`)
			reply.StatusCode = 500
			return reply, nil
		}
		return providerReply("application/json", chatJSON()), nil
	})}})
	if _, err = client.Stream(context.Background(), providerRequest(), nil); err != nil || calls != 2 {
		t.Fatalf("retry failed %d %v", calls, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err = client.Stream(ctx, providerRequest(), func(core.Delta) { cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel ignored: %v", err)
	}
}

func TestProviderResponsesReasoningReplayAndCompletionOnlyContent(t *testing.T) {
	reason := map[string]any{"id": "reason-1", "type": "reasoning", "encrypted_content": "settled-opaque", "summary": []any{map[string]string{"type": "summary_text", "text": "summary"}}}
	message := map[string]any{"id": "message-1", "type": "message", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": "answer"}}}
	envelope := map[string]any{"id": "response-1", "status": "completed", "output": []any{reason, message}, "usage": map[string]any{"input_tokens": 12, "output_tokens": 5, "input_tokens_details": map[string]int{"cached_tokens": 3}}}
	terminalReason := map[string]any{"id": "reason-1", "type": "reasoning", "encrypted_content": "partial-opaque", "summary": reason["summary"]}
	envelope["output"] = []any{terminalReason, message}
	stream := frame("response.created", map[string]any{"type": "response.created", "response": map[string]string{"id": "response-1"}}) +
		frame("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "reason-1", "type": "reasoning", "encrypted_content": "partial-opaque", "summary": []any{}}}) +
		frame("response.reasoning_summary_text.delta", map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": 0, "delta": "sum"}) +
		frame("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": reason}) +
		frame("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 1, "item": map[string]any{"id": "message-1", "type": "message", "role": "assistant", "content": []any{}}}) +
		frame("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 1, "item": message}) +
		frame("response.completed", map[string]any{"type": "response.completed", "response": envelope})
	client, _ := NewProvider(ProviderOptions{API: "openai-responses", APIKey: "fixture-secret", HTTPClient: &http.Client{Transport: providerRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/responses" {
			t.Fatal(r.URL.Path)
		}
		return providerReply("text/event-stream", stream), nil
	})}})
	var text, thought strings.Builder
	response, err := client.Stream(context.Background(), providerRequest(), func(d core.Delta) {
		if d.Type == "text" {
			text.WriteString(d.Text)
		}
		if d.Type == "thinking" {
			thought.WriteString(d.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if text.String() != "answer" || thought.String() != "summary" || response.Usage.InputTokens != 9 {
		t.Fatalf("bad stream %s %s %#v", text.String(), thought.String(), response)
	}
	r := providerRequest()
	r.Messages = append(r.Messages, core.Message{Role: "assistant", Content: response.Content, ReplayModel: r.Model, ReplayProvider: r.Provider, ReplayIdentity: response.ReplayIdentity}, core.TextMessage("user", "next"))
	payload, err := client.payload(r)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(core.JSON(payload))
	if !strings.Contains(encoded, "settled-opaque") || strings.Contains(encoded, "partial-opaque") {
		t.Fatalf("wrong completed replay %s", encoded)
	}
	r.Provider = "other-route"
	payload, err = client.payload(r)
	if err != nil {
		t.Fatal(err)
	}
	encoded = string(core.JSON(payload))
	if strings.Contains(encoded, "opaque") || !strings.Contains(encoded, "summary") {
		t.Fatalf("foreign replay not degraded %s", encoded)
	}
}

func TestProviderAnthropicThinkingAndRedactedReplay(t *testing.T) {
	stream := start() + block(0, map[string]string{"type": "thinking", "thinking": "thought", "signature": "signed"}) + stop(0) + block(1, map[string]string{"type": "redacted_thinking", "data": "opaque-redacted"}) + stop(1) + block(2, map[string]string{"type": "text", "text": "answer"}) + stop(2) + finish("end_turn")
	client, err := NewProvider(ProviderOptions{API: "anthropic-messages", BaseURL: "https://gateway.invalid/anthropic/v1", APIKey: "fixture-secret", ThinkingMode: "budget", ThinkingBudget: 1024, HTTPClient: &http.Client{Transport: providerRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/anthropic/v1/messages" || r.Header.Get("x-api-key") != "fixture-secret" || r.Header.Get("Authorization") != "" {
			t.Fatal("wrong Anthropic route/auth")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["thinking"].(map[string]any)["budget_tokens"] != float64(1024) {
			t.Fatal("missing thinking budget")
		}
		return providerReply("text/event-stream", stream), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	r := providerRequest()
	r.Thinking = "high"
	response, err := client.Stream(context.Background(), r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Content[0].Signature != "signed" || response.Content[1].Data != "opaque-redacted" {
		t.Fatal("opaque blocks lost")
	}
	r.Messages = append(r.Messages, core.Message{Role: "assistant", Content: response.Content, ReplayModel: r.Model, ReplayProvider: r.Provider, ReplayIdentity: response.ReplayIdentity}, core.TextMessage("user", "next"))
	payload, err := client.payload(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(core.JSON(payload)), "opaque-redacted") {
		t.Fatal("native redacted replay lost")
	}
	r.Provider = "another-provider"
	payload, err = client.payload(r)
	if err != nil {
		t.Fatal(err)
	}
	data := string(core.JSON(payload))
	if strings.Contains(data, "signed") || strings.Contains(data, "opaque-redacted") || !strings.Contains(data, "thought") {
		t.Fatalf("foreign opaque data leaked %s", data)
	}
}

func TestProviderResponsesRejectsUnannouncedOrRevisedContent(t *testing.T) {
	for _, stream := range []string{
		frame("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": "unexpected"}),
		frame("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]string{"type": "message", "id": "m"}}) + frame("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": "original"}) + frame("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "message", "id": "m", "content": []any{map[string]string{"type": "output_text", "text": "revised"}}}}),
	} {
		_, _, err := parseResponses(strings.NewReader(stream), false, func(core.Delta) {})
		assertCode(t, err, "MALFORMED_RESPONSE")
	}
}

func TestProviderModelDiscoveryAndJSONOptions(t *testing.T) {
	var options ProviderOptions
	if err := json.Unmarshal([]byte(`{"api":"anthropic-messages","base_url":"https://gateway.invalid/custom","api_key":"fixture-secret","thinking_mode":"adaptive","reasoning_efforts":{"off":"","high":"high"}}`), &options); err != nil {
		t.Fatal(err)
	}
	options.HTTPClient = &http.Client{Transport: providerRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/custom/v1/models" || r.URL.Query().Get("limit") != "1000" || r.Header.Get("x-api-key") != "fixture-secret" {
			t.Fatal("wrong discovery request")
		}
		return providerReply("application/json", `{"models":{"request-id":{"id":"canonical-id","display_name":"Friendly","max_input_tokens":200000,"max_tokens":8192,"input":["text","image"]},"ignored":3}}`), nil
	})}
	client, err := NewProvider(options)
	if err != nil {
		t.Fatal(err)
	}
	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "request-id" || models[0].Name != "Friendly" || models[0].ContextWindow != 200000 || models[0].MaxTokens != 8192 {
		t.Fatalf("wrong discovery %#v", models)
	}
	for _, headers := range []map[string]string{{"Authorization": "secret"}, {"Bad Header": "value"}, {"X-Test": "injected\r\nAuthorization: secret"}} {
		_, err = NewProvider(ProviderOptions{API: "openai-completions", APIKey: "fixture-secret", Headers: headers})
		if err == nil {
			t.Fatal("unsafe header accepted")
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatal("credential leaked in error")
		}
	}
}

func TestProviderAnthropicStopReasons(t *testing.T) {
	for _, reason := range []string{"refusal", "pause_turn", "model_context_window_exceeded"} {
		stream := start() + block(0, map[string]string{"type": "text", "text": "partial or refused"}) + stop(0) + finish(reason)
		client, err := NewProvider(ProviderOptions{API: "anthropic-messages", APIKey: "fixture-secret", HTTPClient: &http.Client{Transport: providerRoundTrip(func(*http.Request) (*http.Response, error) { return providerReply("text/event-stream", stream), nil })}})
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Stream(context.Background(), providerRequest(), nil)
		if err != nil || response.StopReason != reason {
			t.Fatalf("%s: %#v %v", reason, response, err)
		}
	}
}
