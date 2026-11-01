# Built-in coding tools

This package implements a fixed Go registry for the terminal coding assistant. It does not load plugins or require Node. The protocol baseline remains DeepSeek Harness commit `687f282e8625c6b8d11ff58211a90524be2e4317`; the active product scope is described in [PARITY.md](../../docs/PARITY.md#范围调整2026-10-30).

`New()` registers `read`, `write`, `edit`, `list_directory`, `search_files`, `standard_bash`, `job_output`, `job_list` and `job_kill`. The application advertises `standard_bash` to the model as `bash` and registers its other fixed modules, including working-directory, skills, questions, planning, delegation, goals, todo, image reading and Web tools. User-configured MCP servers publish replaceable registry groups. Every call passes through schema validation and its handler; MCP servers validate their own forwarded input schemas.

Legacy file/Shell tools, persistent shells, PTY tools, PTC/`run_code`, JS workflows and the Host terminal panel have been removed. They are not alternative APIs in this package.

## Files and permissions

`read` accepts `file_path`, `offset` and `limit`, returning numbered UTF-8 text. `write` and `edit` require a prior observation of an existing target; edits reject ambiguous matches unless `replace_all` is selected. Writes recheck observations after approval, use confined `os.Root` handles and publish synced temporary files by rename. Target symlinks and non-regular files are rejected. Directory/content searches are bounded and do not traverse symlinks, `.git` or `node_modules`.

Writes denied by the standing permission require explicit one-call approval. Unavailable approval, `never`, rejection, cancellation and late approval cannot authorize execution. The immutable workspace root remains the sandbox boundary after `working_directory` changes the current directory.

Fresh bash runs under macOS Seatbelt or Linux bubblewrap for restricted modes. An unavailable backend fails closed; `full-access` deliberately selects ordinary host access. These are filesystem constraints, not network isolation. Process cleanup terminates the process group; children that deliberately escape it remain a limitation. Linux requires its own integration verification, and Windows restricted execution is unsupported.

## Fresh bash and background jobs

The model's `bash` executes a fresh `bash -c` on every call. Cwd and exported variables do not persist; `workdir` applies to that command. The managed environment is documented in [managed_env.md](managed_env.md). Terminal overrides are `NO_COLOR=1 TERM=dumb PAGER=cat GIT_PAGER=cat`.

Every command starts as an owner-scoped job. `run_in_background` returns its id immediately. A foreground command that exceeds `timeoutMs` (default 120000, capped at 600000) continues as a background job. A foreground command that finishes in its wait returns the final output and leaves the registry. Each stream retains a 64,000-byte tail and, after overflow, writes complete output up to 64 MiB to a private spill file when configured; exceeding that cap revokes the complete-output reference.

`job_list`, `job_output` and `job_kill` enforce session ownership. Model output reads share one consuming cursor; waits default to 30 seconds and cap at 600 seconds, with configuration overrides. User `/jobs`, `/jobs ID` and `/jobs kill ID` inspect or stop current-session jobs without consuming the model cursor. Jobs are process-local and stop when dsgo exits.

Completion notices enter the owning session's inbox: a busy agent accepts them at its next step and an idle session can wake automatically, subject to quiet/wake-budget settings. A result actually delivered to an active wait, a model-requested kill and registry teardown suppress redundant notices. User `/jobs kill` may notify. `HasPendingSessionJobs` includes notice handover so one-shot `run` can wait for automatic result handling.

Canonical values are JSON; `RenderShellResult` renders stdout, stderr and status/sandbox markers for the model. A wider one-shot retry of a sandbox-denied command requires justification and approval; it does not widen standing permission.

## Verification

Native tools, output limits, permissions, file observation and jobs have focused tests. macOS enforcement tests need a host that permits Seatbelt creation. HTTP/PTY fixtures and local race tests do not establish real provider or cross-platform compatibility. Current scope does not promise full official Web parity.
