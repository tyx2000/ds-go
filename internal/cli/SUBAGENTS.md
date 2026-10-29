# Local subagent persistence

The fixed Go spawn and fork backends implement the official local continuation
surface against upstream commit `687f282e8625c6b8d11ff58211a90524be2e4317`.
This does not install or host external delegation backends.

Creation records a model-hidden `subagent/descriptor` in the child's own log and
a `subagent/catalog` membership fact in the direct parent's log before model
dispatch. The descriptor records the establishing backend, creation label and
resolved model route. A separate local policy fact preserves the original
permission ceiling. Cold resume applies the narrower of that ceiling and the
child's current permission, so it cannot widen a later user restriction.
Delegated tools always receive approval policy `never`.

`list_agents` reads the direct parent's own catalog without opening or resuming
children. Recursive discovery visits each reachable catalog once in parent-event
order and reports an unavailable or corrupt branch as a diagnostic. Inherited
catalog facts after a manual fork do not become that fork's children. Old dsgo
`subagent/started` records remain visible as unknown-mode diagnostics; they do
not authorize continuation because they never recorded a continuation descriptor.
This is a compatibility boundary, not an official session-format importer.

`send_message` can cold-resume a direct continuable child. Parent membership,
immutable direct lineage, the first own supported descriptor, policy, depth and
selected preset are validated before inbox acceptance. The manager serializes
concurrent restoration and message admission, retains active capacity bounds,
and removes a newly restored resident when admission fails. Discovery and startup
do not replay original prompts or old settlement notices. A resident running
child may send to its direct parent; a cold historical child cannot use that
relationship to wake its parent.

The Go mappings of official `agentPreset` and `allowedAgentPresets` are
`subagent_agent_preset` and `subagent_allowed_agent_presets`. Both are absent by
default. A configured allowlist alone exposes the model's `agent_preset` enum.
Only spawn supports selection; fork rejects it. The available Go definitions are
the four fixed presets. An independent selection writes `agent-preset/selected`
before the first own descriptor. On cold restore it keeps that selected id;
without this own creation-time selection, the child joins its parent's current
preset. An inherited selection or one written after the descriptor does not opt
the child into independent restoration.

Creation also records a durable delegation-depth floor. Cold resume preserves
the original child's floor. Manual conversation forks start at depth zero;
actual delegations write their own incremented floor after any inherited prefix.
Runtime root ownership and the recursion floor are independent. Request context includes
the delegated-scope statement and, for visible delegation tools at their current
numeric cap, the official depth notice. Changing the configured cap affects the
next request and attempt without changing the tool schema.

Structured completion consults `canCompleteStructured` for owned running
descendants, materializing starts, subtree disposal, pending notices and wake-up
work. The Engine's structured scope separately seals dispatch and inbox admission;
the manager snapshot alone is not a completion transaction.

Capacity is shared by live descendants and materializing starts within one
activation root; an unrelated root has its own pool. The default remains eight.

Remaining differences include exact official runtime-context persistence and dedup,
and official historical descriptor persona/tool restrictions and session-file
interoperability. Existing Go tests exercise the local protocol, not the complete
official Web implementation.

Source references:

- [Subagent contract and cold composition](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/subagent/subagent/README.md)
- [Parent catalog](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/subagent/subagent/src/catalog.ts)
- [Descriptor](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/subagent/subagent/src/descriptor.ts)
- [Preset selection and depth context](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/subagent/tool-subagent/README.md)

Regression entry: `go test -race ./internal/cli -run
'Test(SubagentCatalogSurvives|SubagentColdResume|SubagentDescriptorExcludes|DelegatedScope|SubagentPresetSelection|SubagentDepthContext|SubagentStoredFacts|StructuredCompletionWaits)'`.
