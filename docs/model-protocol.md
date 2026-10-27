# Model protocols and official account transport

The Go implementation in `internal/model` targets the Messages wire protocol
used by official DeepSeek Harness commit
`687f282e8625c6b8d11ff58211a90524be2e4317`. It is a transport implementation,
not a claim of complete Harness compatibility. No live credentialed model or
account login test has been performed; protocol tests use local `httptest`
servers and injected HTTP transports.

## Verified upstream contract

- [serialize.ts](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/llm/llm-deepseek/src/serialize.ts):
  `model`, `stream: true`, `max_tokens`, `messages`, optional `system` and
  `tools[].input_schema`; thinking is `enabled` with
  `output_config.effort` (`low`, `high`, `max`) or `disabled` for `off`.
  Consecutive same-role turns are merged. Tool results immediately follow their
  calls and precede other user input; malformed historical tool arguments use
  an empty object. Thinking text and signatures are replayed.
- [adapter.ts](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/llm/llm-deepseek/src/adapter.ts)
  and [messages-api.ts](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/llm/llm-deepseek/src/messages-api.ts):
  default base root `https://api.deepseek.com/anthropic`, final `/v1` is reused,
  request path `/messages`, `anthropic-version: 2023-06-01`, redirects refused.
- [provider authentication](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/llm/llm-deepseek/README.md):
  API keys use `x-api-key`; acquired account grants use `x-dsh-auth-token`,
  without a Bearer prefix. These are separate routes upstream. Supplying both
  credentials here is rejected rather than silently selecting one.
- [translate.ts](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/llm/llm-deepseek/src/translate.ts)
  and [sse.ts](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/llm/llm-deepseek/src/sse.ts):
  SSE message/block start, incremental text/thinking/signature/tool JSON,
  block stop, message delta with usage/stop reason, then message stop. A missing
  terminal event is an error; an unterminated SSE frame is never dispatched.
- [Files behavior in the pinned adapter README](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/llm/llm-deepseek/README.md)
  and [official Files API](https://api-docs.deepseek.com/guides/files_api/):
  native `/v1/files` operations and file-referenced Messages requests require
  `anthropic-beta: files-api-2025-04-14`. Uploads are multipart with `file`,
  `expires_after[anchor]=created_at`, and `expires_after[seconds]=604800`.
  Native uploads do not require the OpenAI-compatible route's `purpose` field.

## Implemented behavior

`model.New(Options)` implements `core.Model.Stream`. Empty thinking selection
defaults to `high`; an omitted output limit defaults to 256,000 as in the pinned
adapter. Model identifiers pass through unchanged. This does not assert that
any particular model or gateway accepts those defaults: callers must select a
supported model and an appropriate output cap.

The returned response preserves ordered native `text`, `thinking` (including
`signature`) and `tool_use` blocks, exact valid tool input JSON, native stop
reasons, and the four token usage counters. Tool results support text or arrays
of text/images. Invalid/truncated tool inputs are never turned into callable
completed tools. Truncation at `max_tokens` removes incomplete tool calls;
failed/cancelled partial responses retain unfinished argument bytes in the
block's `data` string so the response remains valid JSON for storage.

The callback receives `block_start`/`block_stop`, `text`, `thinking`, `signature`,
and `tool_input` deltas with dense block indices. `block_start.text` identifies
the native block type. Callers must not execute tools from deltas: only a
successful complete response is eligible for tool dispatch. Callback calls are
synchronous; cancellation takes effect when control returns to the client.

SSE accepts LF, CRLF and CR delimiters, UTF-8 split across network reads,
comments, multiline data, and ignorable future non-content event kinds.
Duplicate/open/closed block errors and mismatched event types fail explicitly.
An `application/json` full message is also accepted for compatible gateways.

Errors expose stable codes and HTTP status, without retaining or displaying
provider error bodies, credentials, or URLs. Redirects are rejected even with
an injected HTTP client. At most two retries occur by default for transient
failures before a content block has started; once any content block is observed,
errors return the partial response with no replay. Backoff and `Retry-After`
are cancellable and bounded. Negative `MaxRetries` disables retries. A default
five-minute activity watchdog cancels a stalled request.

Images are supplied as prepared native `image.source` values: `base64` with
`media_type`/`data`, or `file` with `file_id`. The client uploads base64 images
before inference, caches up to 64 references per immutable client/credential
configuration, requests seven-day expiry, and refreshes within an hour of its
locally tracked expiry. A Files failure rebuilds the request with inline bytes;
a stale uploaded reference permits one fallback before any inference output.
`DisableFiles` opts into inline input. `UploadImage` and `DeleteFile` are explicit
helpers. Remote deletion is never performed implicitly as quota cleanup.

Local bounds are 64 MiB per upload, 128 MiB accumulated raw image bytes, 600
images, 20 MiB accumulated base64 characters on fallback, 192 MiB encoded
request, 64 MiB response, 8 MiB SSE frame, and 4,096 response blocks. The upload
bound deliberately follows the official public Files documentation's 64 MiB
limit; it is a local limit, not a statement that all upstream configurations
use the same maximum. Uploads have a one-minute request deadline.

## Request controls and replay identity

`core.Request` carries temperature, stop sequences, user/session attribution,
compaction attribution, capability-selected in-history system updates, tool
addition/removal events and deferred declarations. The direct DeepSeek client
translates supported updates and sends the corresponding capability beta
headers. Unsupported update configurations fail before dispatch.

`core.Message.ReplayModel` and `ReplayProvider` are storage metadata; neither is
sent as a wire message property. A different provider or model loses signatures,
redacted thinking and opaque reasoning source state. Visible reasoning text is
retained as provider-neutral history. Legacy sessions without provider metadata
are treated as direct DeepSeek history, so a generic provider cannot replay
their opaque state. An application changing the endpoint behind the same route
identifier must also invalidate or change that route's replay identity.

## Official DeepSeek account

`NewAccount(AccountOptions)` implements the fixed official Platform protocol:
S256 PKCE, a random loopback listener, constant-time state checks,
`auth_init`/`auth_exchange`, save-before-success redirect, cancellation proof,
profile and logout. Account grants retain issuer and device identity and belong
only in the private credential store. Browser URLs must have the configured
Platform origin and expected authorization paths. The default origin is
`https://platform.deepseek.com`; insecure HTTP is only available through the
explicit loopback testing option. There is no invented refresh-token flow.

`Client.ListAccountModels(ctx)` uses the inference origin's `GET /models` with
the account grant. It keeps unsupported rows with a reason, applies the pinned
client-version ceiling, and exposes modality/capability metadata. `BuiltinModels`
is the pinned API-key route's advisory catalog, not a probe of live key access.

## Official pi-ai wire protocols

The pinned [`llm-pi-ai` documentation](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/llm/llm-pi-ai/README.md)
supports three explicit gateway wire APIs. `NewProvider(ProviderOptions)` ports
these protocols through the same `core.Model` interface:

| `api` | Inference suffix | Authentication |
| --- | --- | --- |
| `openai-completions` | `/chat/completions` | Bearer API key |
| `openai-responses` | `/responses` | Bearer API key |
| `anthropic-messages` | `/messages` | `x-api-key` and Anthropic version |

The configured `base_url` is unchanged except for a trailing slash; it does not
inherit the direct DeepSeek client's automatic `/v1` suffix. Empty bases use
the protocol's official API root. A configured `api_key_env` resolves at each
request and takes precedence over `api_key`; an empty reference fails before
network I/O. Ambient AWS, Google, Azure or OAuth credential discovery is not
simulated. Custom headers cannot replace authentication, framing, host or
transport headers. Redirects are refused and error messages omit remote bodies
and credentials.

Example provider-profile value, with a credential reference rather than a key:

```json
{
  "api": "openai-completions",
  "base_url": "https://gateway.example/v1",
  "api_key_env": "GATEWAY_API_KEY",
  "reasoning_efforts": {"off": "", "high": "high"},
  "max_tokens_field": "max_completion_tokens",
  "system_role": "developer"
}
```

Model-specific Anthropic thinking is opt-in: `thinking_mode: "adaptive"`, or
`thinking_mode: "budget"` with `thinking_budget` of at least 1,024 and below the
request output limit. `reasoning_efforts` maps offered levels to protocol
spellings. The Chat switches `requires_reasoning_content`,
`omit_streaming_usage`, `max_tokens_field` and `system_role`, plus Responses'
`omit_max_output_tokens`, cover the declared compatibility options implemented
here. These are a finite subset of pi-ai's compatibility configuration.

The provider clients preserve text, visible reasoning, function-call arguments,
refusals and token usage. Responses retains completed reasoning items for
stateless replay; an in-progress encrypted item is replaced by its settled
`response.output_item.done` value. The stream requires a terminal event and
rejects revisions to text already emitted. Interrupted output returns its
partial response and is never automatically replayed. Defaults allow five
transient retries before content; a negative `max_retries` disables retries.
The common pi-ai seam rejects stop sequences with `UNSUPPORTED_OPTION`, as
upstream does. Current wire references are the official
[Chat streaming events](https://developers.openai.com/api/reference/resources/chat/subresources/completions/streaming-events)
and [Responses streaming events](https://developers.openai.com/api/reference/resources/responses/streaming-events).
Anthropic's `refusal`, `pause_turn` and `model_context_window_exceeded` stop
reasons remain explicit rather than being reported as malformed streams.
The caller owns continuation and truncation presentation, following the
[native stop-reason contract](https://platform.claude.com/docs/en/build-with-claude/handling-stop-reasons).

`ProviderClient.ListModels(ctx)` offers endpoint discovery without changing
configuration. It reads standard `data` arrays and enriched `models` maps,
keeps a map key as the requested model id, and normalizes names, token limits
and input modalities. Only Anthropic discovery normalizes the `/v1` segment
and adds `limit=1000`; inference still uses the configured base unchanged.

## Remaining gaps

- pi-ai's complete installed provider/model catalog, provider-native OAuth,
  ambient credential discovery, AWS signing, Azure/Vertex native routes,
  per-model catalog overrides and its full compatibility option vocabulary.
  The three explicit protocols are not a claim that every pi-ai provider works.
- Exact pi-ai native replay envelopes, cross-protocol tool-id rewriting and
  in-history dynamic tool updates for the generic provider paths. The direct
  DeepSeek path supports its declared update capability; generic unsupported
  content is rejected instead of silently serialized as if equivalent.
- Provider-native server tools, audio/video generation, structured assistant
  images, and other non-text/non-function response blocks. These fail with
  `UNSUPPORTED_CONTENT`; supporting the three wire protocols does not imply
  every optional feature of the underlying provider API is available.
- Attachment storage, deterministic resizing/re-encoding, per-model image token
  pricing, image-offload events/placeholders, attachment handle text or path
  attribution. Callers provide already prepared image bytes. External image URLs
  and assistant image output are rejected.
- Persistent credential-scoped Files indexes, cross-process upload locking,
  singleflight, remote file listing/retrieval, quota-based cleanup, and exact
  upstream oldest-prefix image offload policy. Cache reuse is local and bounded;
  concurrent identical first uploads may both reach the provider.
- Provider-specific `dsh_session_log` / `dsh_plugin_packages` request extensions.
  The client identifies itself as dsgo rather than impersonating official
  Harness. Common attribution headers do not imply those extension payloads.
- Upstream's full request retry policy and durable attempt settlement. This
  client retries only before output; the agent/session layer owns recorded
  attempts and further recovery.

These gaps must remain explicit in the feature matrix. Passing offline protocol
tests does not establish whole-product parity or confirm access to a live model.
