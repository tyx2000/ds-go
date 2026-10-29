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

## Prompt line editor

When stdin and stdout are terminals, `dsgo` prompts read through `LineEditor`
instead of the kernel's canonical line editing, which erases one byte and one
column per Backspace and so leaves half of a CJK character on screen and
partial UTF-8 in the line. The editor turns off echo and canonical mode for the
duration of one line (signal keys keep working), deletes whole characters and
erases their display width. It asks the terminal for the starting column so
that deletions across wrapped rows redraw from the first input cell; without
an answer it erases in place. Left/Right, Home/End, Ctrl+A/E/B/F, Delete and
Backspace edit at the caret. Ctrl+Left/Right or Alt+B/F move by words;
Ctrl+W/Alt+Backspace delete the previous word, Alt+D deletes the next word,
Ctrl+U/K delete before/after the caret. Ctrl+D on an empty line ends input,
and Ctrl+C clears the draft. Combining marks and common emoji sequences move
and delete together. Bracketed paste preserves newlines without submitting.

History is opt-in per read, bounded to 200 in-memory entries, and never written
to a history file. Up/Down restore a partially typed draft after browsing.
`ReadLineWithOptions` captures the prompt, history policy and completion provider
for one broker request, so approval/question reads can disable both history and
completion. Providers receive rune offsets and return a replacement range and
display candidates. Tab opens a multi-candidate menu; arrows choose, Tab/Enter
accept, and Escape/Shift+Tab dismiss without changing the draft. A single match
completes immediately. Candidate labels are sanitized before display.

`WriteAbove` coordinates asynchronous output with draft/caret restoration and
keeps streamed fragments together above the prompt. Its caller must not hold the
raw writer's lock. `Resize` and Unix SIGWINCH redraw the region at the current
width. Full redraw needs an ANSI terminal and a cursor-position response;
plain terminals retain in-place editing and print fallback completion rows.
Pipes keep plain line reading. A `TERM=dumb` terminal is not sent the
position query and always erases in place; inside Emacs (`INSIDE_EMACS`),
where Emacs edits the line, prompts keep canonical reading. On a color-capable
terminal the `dsgo>` and `...` prompts are bold cyan so input stands apart from
output; `NO_COLOR`, dumb terminals and pipes keep them plain.

The browser's complete reference-chip lifecycle, command-specific popups,
group/drill-down menus and all composer settings are still separate alignment
work. The terminal completion provider inserts ordinary text and never reads a
file's contents merely because its path was selected. Sources:
[ui-input-trigger](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/client/ui-input-trigger/README.md),
[ui-commands](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/client/ui-commands/README.md).
