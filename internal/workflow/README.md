# Workflow terminal port

Pinned upstream: `687f282e8625c6b8d11ff58211a90524be2e4317`.
The official consumer is `packages/workflow/tool-workflow/src/index.ts`; runtime semantics
are from `packages/workflow/workflow-ptc/src/{index,meta,runtime,realm,host}.ts`.
The port keeps Go as the application/ownership layer and uses the same existing Node
runtime requirement as PTC for the model-written JavaScript body. It does not load
Harness extension packages.

Implemented:

- Official `workflow` arguments (`script`, `meta`, optional object `args`,
  `run_in_background`); metadata and JavaScript syntax are checked before a run exists.
- `agent`, `parallel`, `pipeline`, `phase`, `log`, `args`, top-level await/return;
  child provider/model overrides; strict supported option and schema vocabulary.
- FIFO concurrency (auto min(16,max(1,cores-2))), 1000 lifetime child calls,
  4096 items per combinator, 5000 ms initial synchronous VM slice, no overall deadline.
- Ordinary failed children and stage exceptions become null; hook misuse remains
  fatal through the combinators. Lossy/non-JSON/exotic/circular return values fail.
- Child attribution, fresh sessions, inherited policy/preset/model/CWD, and cleanup
  before settlement. Foreground follows its tool-step signal; background runs belong
  to the application lifetime and appear through the existing jobs interface.
- Durable `tool-workflow/run-start`, `agent-start`, `agent-end`, `run-end` display
  records with continuous-prefix failure behavior, preserving omitted versus empty
  phases. Terminal replay marks a missing terminal tail interrupted.

Official workflows do **not** checkpoint script state or resume/re-execute after a
restart. Durable records are observations for presentation. The Go port follows that
boundary; `/workflows` reads them without rerunning model work.

Remaining alignment work:

- Structured children currently receive a JSON-output instruction and their final
  JSON is checked against the official schema subset. The official subagent seam
  uses an output-schema capability, including its model-native structured-output
  request and retry behavior; that requires shared model/agent protocol support.
- Background completion uses the current dsgo job notices/output ring rather than
  upstream's exact job-event wire format. Completed job retention is process-local.
- The terminal display exposes phase/member status and child ids; full interactive
  same-parent child navigation and disclosure keyboard shortcuts remain TUI work.

Tests exercise actual Node guest execution, fatal/null discipline, FIFO caps,
parse-before-publication, lossless JSON, synchronous timeout, cancellation/drain,
member pairing, fresh child metadata, background owner isolation and durable replay.
