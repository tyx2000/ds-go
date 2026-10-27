package model

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dsgo/internal/core"
)

func request() core.Request {
	return core.Request{Model: "fixture-model", Messages: []core.Message{core.TextMessage("user", "hello")}, MaxTokens: 1024, Thinking: "high"}
}
func frame(kind string, value any) string {
	data, _ := json.Marshal(value)
	return "event: " + kind + "\ndata: " + string(data) + "\n\n"
}
func start() string {
	return frame("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "m1", "type": "message", "usage": map[string]int{"input_tokens": 7, "cache_read_input_tokens": 2}}})
}
func block(index int, value any) string {
	return frame("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": value})
}
func delta(index int, value any) string {
	return frame("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": value})
}
func stop(index int) string {
	return frame("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
}
func finish(reason string) string {
	return frame("message_delta", map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": reason}, "usage": map[string]int{"output_tokens": 9}}) + frame("message_stop", map[string]string{"type": "message_stop"})
}
func textStream() string {
	return start() + block(0, map[string]any{"type": "text", "text": ""}) + delta(0, map[string]string{"type": "text_delta", "text": "你好"}) + stop(0) + finish("end_turn")
}
func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("error = %v; want %s", err, code)
	}
}

func TestNativePayloadAndSSEThinkingToolUse(t *testing.T) {
	var received map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/anthropic/v1/messages" {
			t.Errorf("path %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "test-key" || r.Header.Get("Authorization") != "" || r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("wrong auth headers")
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		output := start() + block(5, map[string]string{"type": "thinking", "thinking": ""}) + delta(5, map[string]string{"type": "thinking_delta", "thinking": "consider"}) + delta(5, map[string]string{"type": "signature_delta", "signature": "opaque-a"}) + delta(5, map[string]string{"type": "signature_delta", "signature": "-b"}) + stop(5) + block(9, map[string]any{"type": "tool_use", "id": "call-1", "name": "read_file", "input": map[string]any{}}) + delta(9, map[string]string{"type": "input_json_delta", "partial_json": "{\"path\":"}) + delta(9, map[string]string{"type": "input_json_delta", "partial_json": "\"x\"}"}) + stop(9) + finish("tool_use")
		// Fragment every byte to exercise UTF-8, SSE framing and JSON boundaries.
		for _, b := range []byte(output) {
			_, _ = w.Write([]byte{b})
			w.(http.Flusher).Flush()
		}
	}))
	defer server.Close()
	input := request()
	input.System = "system"
	input.Tools = []core.ToolSpec{{Name: "read_file", Description: "read", InputSchema: core.JSON(map[string]any{"type": "object"})}}
	var deltas []core.Delta
	response, err := New(Options{BaseURL: server.URL + "/anthropic/", APIKey: "test-key"}).Stream(context.Background(), input, func(d core.Delta) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatal(err)
	}
	if string(received["thinking"]) != `{"type":"enabled"}` || string(received["output_config"]) != `{"effort":"high"}` || string(received["max_tokens"]) != "1024" {
		t.Fatalf("payload %#v", received)
	}
	if response.ID != "m1" || response.StopReason != "tool_use" || len(response.Content) != 2 || response.Content[0].Thinking != "consider" || response.Content[0].Signature != "opaque-a-b" || string(response.Content[1].Input) != `{"path":"x"}` {
		t.Fatalf("response %#v", response)
	}
	if response.Usage.InputTokens != 7 || response.Usage.OutputTokens != 9 || response.Usage.CacheReadTokens != 2 {
		t.Fatalf("usage %#v", response.Usage)
	}
	if len(deltas) < 8 {
		t.Fatal("missing incremental deltas")
	}
}

func TestSerializeReplayAndImmediateToolResults(t *testing.T) {
	input := request()
	input.Thinking = "off"
	input.Messages = []core.Message{core.TextMessage("user", "work"), {Role: "assistant", Content: []core.Block{{Type: "thinking", Thinking: "thought", Signature: "signature"}, {Type: "tool_use", ID: "c1", Name: "bash", Input: json.RawMessage(`[1]`)}}}, {Role: "user", Content: []core.Block{{Type: "text", Text: "next"}, {Type: "tool_result", ToolUseID: "c1", Content: "done", IsError: true}}}}
	wire, err := serialize(input)
	if err != nil {
		t.Fatal(err)
	}
	if wire.Thinking["type"] != "disabled" || wire.OutputConfig != nil {
		t.Fatal("off thinking mapped incorrectly")
	}
	if wire.Messages[1].Content[0].Signature != "signature" || string(wire.Messages[1].Content[1].Input) != "{}" {
		t.Fatal("replay not preserved")
	}
	if wire.Messages[2].Content[0].Type != "tool_result" {
		t.Fatal("results must precede text")
	}
	input.Messages = input.Messages[:2]
	_, err = serialize(input)
	assertCode(t, err, "INVALID_REQUEST")
}

func TestNonStreamingJSONAndAccountToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-dsh-auth-token") != "account-grant" || r.Header.Get("x-api-key") != "" {
			t.Error("wrong endpoint/auth")
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fmt.Fprint(w, `{"id":"m1","type":"message","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`)
	}))
	defer server.Close()
	response, err := New(Options{BaseURL: server.URL + "/v1", Token: "account-grant"}).Stream(context.Background(), request(), nil)
	if err != nil || len(response.Content) != 1 || response.Content[0].Text != "ok" {
		t.Fatalf("%#v %v", response, err)
	}
}

func TestRetryBeforeContentButNeverAfterPartialOutput(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					if partial {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, start()+block(0, map[string]string{"type": "text", "text": "partial"}))
						return
					}
					w.WriteHeader(503)
					fmt.Fprint(w, `{"error":{"message":"key=secret and prompt"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, textStream())
			}))
			defer server.Close()
			response, err := New(Options{BaseURL: server.URL, APIKey: "secret", RetryDelay: time.Millisecond}).Stream(context.Background(), request(), nil)
			if partial {
				assertCode(t, err, "STREAM_CLOSED")
				if calls.Load() != 1 || response.Content[0].Text != "partial" {
					t.Fatalf("partial %#v calls %d", response, calls.Load())
				}
			} else if err != nil || calls.Load() != 2 {
				t.Fatalf("calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestCancellationRetainsSerializablePartialTool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, start()+block(0, map[string]any{"type": "tool_use", "id": "c", "name": "bash", "input": map[string]any{}})+delta(0, map[string]string{"type": "input_json_delta", "partial_json": "{\"command\":"}))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	response, err := New(Options{BaseURL: server.URL, APIKey: "k"}).Stream(ctx, request(), func(d core.Delta) {
		if d.Type == "tool_input" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v", err)
	}
	if len(response.Content) != 1 || response.Content[0].Data != `{"command":` {
		t.Fatalf("partial %#v", response)
	}
	if _, err := json.Marshal(response); err != nil {
		t.Fatal("partial cannot be persisted", err)
	}
}

func TestMalformedSSEAndUnterminatedTail(t *testing.T) {
	cases := map[string]string{"mismatched event": "event: ping\ndata: {\"type\":\"message_start\"}\n\n", "unclosed block": start() + block(0, map[string]string{"type": "text", "text": "x"}) + finish("end_turn"), "delta after stop": start() + block(0, map[string]string{"type": "text", "text": "x"}) + stop(0) + delta(0, map[string]string{"type": "text_delta", "text": "x"}), "unterminated": start() + block(0, map[string]string{"type": "text", "text": "x"}) + stop(0) + strings.TrimSuffix(finish("end_turn"), "\n\n")}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := parseSSE(strings.NewReader(body), func(core.Delta) {})
			if err == nil {
				t.Fatal("accepted malformed stream")
			}
		})
	}
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		body := strings.ReplaceAll(textStream(), "\n", ending)
		response, _, err := parseSSE(strings.NewReader(body), func(core.Delta) {})
		if err != nil || response.Content[0].Text != "你好" {
			t.Fatalf("line ending %q: %v", ending, err)
		}
	}
}

func TestHTTPErrorRedactionAndRedirectRefusal(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	_, err := New(Options{BaseURL: server.URL, APIKey: "a-secret"}).Stream(context.Background(), request(), nil)
	assertCode(t, err, "HTTP_307")
	if targetCalls.Load() != 0 {
		t.Fatal("followed credential redirect")
	}
	errorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":{"message":"a-secret prompt contents","type":"authentication_error"}}`)
	}))
	defer errorServer.Close()
	_, err = New(Options{BaseURL: errorServer.URL, APIKey: "a-secret"}).Stream(context.Background(), request(), nil)
	assertCode(t, err, "AUTH")
	if strings.Contains(err.Error(), "a-secret") || strings.Contains(err.Error(), "prompt") {
		t.Fatalf("leaked error: %v", err)
	}
}

func TestFilesUploadReuseAndInlineFallback(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			var uploads, requests atomic.Int32
			image := []byte("prepared-fixture-image")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/files" {
					uploads.Add(1)
					if r.Header.Get("anthropic-beta") != filesBeta {
						t.Error("missing files beta")
					}
					if err := r.ParseMultipartForm(1 << 20); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					if r.FormValue("expires_after[anchor]") != "created_at" || r.FormValue("expires_after[seconds]") != "604800" {
						t.Error("wrong expiry")
					}
					file, _, err := r.FormFile("file")
					if err != nil {
						t.Error(err)
						return
					}
					defer file.Close()
					data, _ := io.ReadAll(file)
					if !reflect.DeepEqual(data, image) {
						t.Error("changed bytes")
					}
					if fail {
						w.WriteHeader(503)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"id":"file-1","type":"file","size_bytes":22,"created_at":"2026-10-01T00:00:00Z","filename":"image.png","mime_type":"image/png"}`)
					return
				}
				requests.Add(1)
				var payload struct {
					Messages []core.Message `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				want := "file"
				if fail {
					want = "base64"
				}
				if got := sourceOf(&payload.Messages[0].Content[1])["type"]; got != want {
					t.Errorf("image source %s want %s", got, want)
				}
				if !fail && r.Header.Get("anthropic-beta") != filesBeta {
					t.Error("missing message files beta")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, textStream())
			}))
			defer server.Close()
			input := request()
			input.Messages[0].Content = append(input.Messages[0].Content, core.Block{Type: "image", Source: map[string]string{"type": "base64", "media_type": "image/png", "data": base64.StdEncoding.EncodeToString(image)}})
			client := New(Options{BaseURL: server.URL, APIKey: "key"})
			for i := 0; i < 2; i++ {
				if _, err := client.Stream(context.Background(), input, nil); err != nil {
					t.Fatal(err)
				}
			}
			if requests.Load() != 2 {
				t.Fatal("missing requests")
			}
			if !fail && uploads.Load() != 1 {
				t.Fatalf("did not reuse upload: %d", uploads.Load())
			}
		})
	}
}

func TestIdleTimeoutAndValidationBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()
	client := New(Options{BaseURL: server.URL, APIKey: "k", IdleTimeout: 20 * time.Millisecond, MaxRetries: -1})
	_, err := client.Stream(context.Background(), request(), nil)
	assertCode(t, err, "TIMEOUT")
	bad := request()
	bad.Thinking = "super"
	_, err = client.Stream(context.Background(), bad, nil)
	assertCode(t, err, "UNSUPPORTED_REASONING_EFFORT")
	if calls.Load() != 1 {
		t.Fatalf("invalid request reached network: %d", calls.Load())
	}
}

func TestInHistoryUpdatesAttributionAndReplayScope(t *testing.T) {
	temperature := 0.3
	input := request()
	input.Temperature = &temperature
	input.StopSequences = []string{"STOP"}
	input.UserID = "u1"
	input.SessionID = "s1"
	input.Purpose = "compaction"
	input.SystemPromptUpdate = "in-history"
	input.ToolUpdate = "in-history"
	input.Tools = []core.ToolSpec{{Name: "bash", InputSchema: json.RawMessage(`{"type":"object"}`), DeferLoading: true}}
	input.Messages = []core.Message{core.TextMessage("system", "initial"), core.TextMessage("user", "first"), {Role: "assistant", ReplayModel: "old-model", Content: []core.Block{{Type: "thinking", Thinking: "", Signature: "old-signature"}, {Type: "text", Text: "answer"}}}, core.TextMessage("system", "new"), {Role: "developer", Content: []core.Block{{Type: "tool-addition", ToolName: "bash"}}}, core.TextMessage("user", "next")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-deepseek-harness-user-id") != "u1" || r.Header.Get("x-deepseek-harness-session-id") != "s1" || r.Header.Get("x-deepseek-harness-compact") != "1" || r.Header.Get("anthropic-beta") != "mid-conversation-tool-changes-2026-07-01" {
			t.Error("missing attribution/tool-update headers")
		}
		var data map[string]any
		if json.NewDecoder(r.Body).Decode(&data) != nil {
			t.Error("bad request")
			return
		}
		if data["temperature"] != temperature || data["system"] != "initial" {
			t.Errorf("wrong request %#v", data)
		}
		messages := data["messages"].([]any)
		if len(messages) != 5 || messages[2].(map[string]any)["role"] != "user" || messages[3].(map[string]any)["role"] != "system" || messages[4].(map[string]any)["role"] != "system" {
			t.Errorf("system updates ordered incorrectly: %#v", messages)
		}
		thought := messages[1].(map[string]any)["content"].([]any)[0].(map[string]any)
		if thought["thinking"] != "" || thought["signature"] != nil {
			t.Errorf("foreign replay signature retained %#v", thought)
		}
		tool := messages[4].(map[string]any)["content"].([]any)[0].(map[string]any)
		if tool["type"] != "tool_addition" || tool["tool"].(map[string]any)["name"] != "bash" {
			t.Errorf("wrong tool reference %#v", tool)
		}
		if _, ok := messages[1].(map[string]any)["replay_model"]; ok {
			t.Error("replay metadata leaked to provider")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, textStream())
	}))
	defer server.Close()
	if _, err := New(Options{BaseURL: server.URL, APIKey: "k"}).Stream(context.Background(), input, nil); err != nil {
		t.Fatal(err)
	}
	input.Messages = append(input.Messages, core.TextMessage("system", ""))
	if _, err := serialize(input); err == nil {
		t.Fatal("accepted empty in-history update")
	}
}
