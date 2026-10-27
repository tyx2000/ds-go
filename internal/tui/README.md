# Conversation trajectory in the terminal

This package projects durable dsgo Session events into the official Web usage
surfaces represented by `ui-trajectory`, turn outline and session statistics.
Source baseline: `687f282e8625c6b8d11ff58211a90524be2e4317`, notably
`packages/client/ui-trajectory/README.md`.

`/trajectory` opens the full-screen pager when stdin/stdout are terminals. Pipes
receive stable text; `--json` emits a structured projection. `/trajectory N` or
`/trajectory turn N` selects one turn, with full input/output details in plain
mode. `/trajectory stats` renders cumulative session counts and recorded usage.
`--plain` explicitly selects text output.

The pager supports arrows or j/k, Page Up/Down, Home/End, turn navigation with
brackets, Enter to inspect a record, Tab to cycle Summary/Input/Output/Timing/Raw/
Code, `/` search and n/N next/previous match, `s` statistics, `u` turn outline,
and q to restore the original screen. It refreshes the durable tail, stops following
when the user scrolls upward, and renders only the current screen. Terminal flags,
cursor and alternate screen are restored on quit and cancellation. Recorded model
content is stripped of terminal controls, including OSC clipboard/title sequences.

Projection preserves separate turn/request/tool completion; pending historical
records become interrupted without making up a finish time. Nested PTC calls pair
by durable parent/call identity, with an inference fallback for older records.
Only explicitly recorded first/last-token facts yield TTFT/generation/rate values;
no real-time elapsed spans or prices are invented. Raw arguments use `input_raw`
when recorded, preserving original whitespace. Legacy compacted JSON cannot recover
formatting that was never saved. Compaction remains in chronological position,
including between-turn requests. Failed partial assistant attempts remain inspectable.

Remaining interface alignment: the browser's graphical zoom/pan/interval timing
overview, image lightbox and authorized media loading, richer JSON-tree folding,
code highlighting/copy controls, and independently paged old-history loading are
not implemented here. The resident projection reads the complete dsgo event log;
only terminal rendering is windowed. This is one set of conversation inspection
surfaces, not a complete replacement for every Web panel or composer interaction.

Tests cover projection, counters, failed/partial/recovered rows, PTC concurrency
pairing, exact recorded arguments, unknown timing, control injection, CJK wrapping,
keyboard/search navigation and a real PTY quit/cancel restoration check.
