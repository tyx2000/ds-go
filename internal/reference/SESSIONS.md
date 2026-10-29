# Session references

`Sessions` implements the fixed official
[`session-reference`](https://github.com/deepseek-ai/deepseek-harness/tree/687f282e8625c6b8d11ff58211a90524be2e4317/packages/context/session-reference)
contract over dsgo's local store. It adds no model-facing search tool.

- `Candidates` reads metadata and optional lightweight label projections, excludes
  the target, and ranks same-workspace records before unlocated and other records.
  It never reads a source log during completion.
- Canonical mentions encode `JSON.stringify(sessionId)` with unpadded base64url.
  The codec rejects alternate encodings and preserves opaque lone-surrogate ids
  as WTF-8 in Go; the actual store still enforces its own id vocabulary.
- `Prepare` rewrites direct-user mentions to readable labels and returns an
  adjacent, sourced user-context snapshot. It reads each distinct source once,
  captures its current compacted surface and final observed sequence, and excludes
  reasoning, tools, agent messages and previously injected references. An atomic
  `user/input` batch contributes only its direct first message.
- Up to three distinct sources are accepted. Each preview has its own exact
  serialized JSON-byte budget: explicit bytes, or a 64 KiB floor and the configured
  fraction of the target model's context. Checkpoints and the newest message are
  retained while older ordinary messages are dropped; remaining text is shortened
  at UTF-8 boundaries with exact omission counts.
- Truncated sources optionally save the same captured full projection under the
  target's ownership. Missing storage and failed saves stay explicit. The full
  transcript uses JSON string fragments of at most 64 Unicode code points per
  line, preserving original text and newlines. Cancellation prevents publication
  even when the optional backend ignores cancellation and writes later.

The caller invokes `Prepare` at actual input admission and persists its returned
direct message and contexts in one ordered transaction. Queueing must not capture
early, and replay must never prepare stored context again. The caller supplies
the target's effective model capacity and adapts spill ownership metadata.

`capturedFormatVersion: 1` identifies dsgo's current local event generation; this
module does not claim to import official `session.vN.jsonl` files. The local
compaction event is projected as its current surface and canonical checkpoints
are recognized by `source.kind == "compact-checkpoint"`.

Regression entry: `go test -race ./internal/reference -run TestSession -count=1`.
