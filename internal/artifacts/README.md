# Deliverables, output spills and workspace changes

This package implements the fixed official deliverable contract from DeepSeek
Harness [`687f282e8625c6b8d11ff58211a90524be2e4317`](https://github.com/deepseek-ai/deepseek-harness/tree/687f282e8625c6b8d11ff58211a90524be2e4317/packages/deliverables).
It loads no plugins.

`present` accepts `files: [{path, description?}]`, with 1–8 regular files per
call. Relative paths resolve against the calling session's current directory.
Accessible absolute source paths are allowed, including paths outside the
workspace. A final symlink, missing file, or directory fails validation. Successful
calls append `deliverables/presented` with the turn number, call ID and resolved
absolute source paths. Contents remain at their source path; later edits change
what the user opens. Nested calls belong to their calling session. The model sees
`Presented PATH` lines; a PTC program receives the canonical `{turn, files}` value.

Spills retain full returned tool text in a private, durable file, with a bounded
head/tail display and a recovery path. `/artifacts spill ID [OFFSET] [LIMIT]`
pages through the original output. Metadata binds each ID to its owning session;
SHA256 validates bytes on every recovery read. Offsets are byte positions and
page boundaries preserve complete UTF-8 characters. `read_file` and `read` are
excluded to prevent recursive spilling during recovery. Storage failures leave
the original inline content available and report a diagnostic. Failures to append
the durable spill audit event still stop the turn.

The baseline's default retention budget is 12,500 estimated tokens. The current
CLI adapter uses a 50,000-byte approximation. This policy runs only on the
model-facing top-level copy; canonical nested program values remain intact.
Upstream image accounting and retention ordering, token-exact estimation,
old-result pruning, dispatch-only copies and capture before individual tool
output caps remain separate work. A spill cannot restore bytes already discarded
inside a tool before it returns.

Workspace comparisons take before/after snapshots under the session turn lock,
including pre-existing staged and untracked working files, new files, deletions,
mode changes, and binary files. No Git command runs and the index remains intact.
Snapshots do not traverse symlinks or `.git`. Reads use `os.Root`, so concurrent
path replacement cannot escape the workspace. Delegated sessions do not create
their own turn summaries. A successful saved comparison appends
`workspace/changes: {turn}`; `/artifacts changes [TURN_ID]` and
`/artifacts diff TURN_ID INDEX` retrieve its summary and comparison.

The official limits of 500 displayed files, 2 MiB per comparable file and 100 ms
per line comparison are retained. A larger file is listed as `oversized`; NUL
bytes mark a file as `binary`. Expensive comparisons fall back to whole-file
replacement and set `coarse`. Scans also have finite local safeguards of 100,000
files, 256 MiB read and 30 seconds. Preview failure does not fail the underlying
model turn. The Go implementation keeps summaries and content-addressed bytes
durably; official summaries are held for the live host session. Repository files
above the workspace and tool edits outside it are not yet captured. Exact official
Git ignore/rename semantics are not claimed by this filesystem implementation.

Focused verification:

```sh
go test -race ./internal/artifacts ./internal/cli -run 'Test(Spill|Present|Workspace|Snapshot|Artifact)' -count=1
```
