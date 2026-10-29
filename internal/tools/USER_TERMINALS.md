# Host user terminals

The fixed source baseline is DeepSeek Harness
`687f282e8625c6b8d11ff58211a90524be2e4317`:

- `packages/api/terminal-controller/src/{index,terminal,retention,shells}.ts`
- `packages/client/ui-sidebar-terminal/README.md`
- `packages/subprocess/subprocess-local/src/{terminal,shell-activity,process-inspector}.ts`

`/terminal` opens the selected Session's full-screen terminal list. `n` opens the
remembered available shell; `s` discovers installed shells and opens the selected
one. Enter attaches to an existing identity, `r` renames it and `c` closes it.
Inside a terminal, Ctrl+] returns to the conversation or terminal list. Ctrl+C,
Tab, shell completion, arrow keys and other input go to the shell. Shift+PageUp
and Shift+PageDown inspect retained scrollback. Raw terminal attributes, input
modes, cursor and alternate screen are restored on return and cancellation.

The same operations are available as `/terminal list`, `shells`, `new
[SHELL_PATH]`, `attach ID`, `rename ID TITLE` and `close ID`. Pipes can inspect
metadata and explicitly create/close terminals; attaching requires a TTY.

The Host-only controller is separate from the model tool Registry and the
minimal preset's private persistent Bash. Explicit user terminals use the OS
user's permissions independently of Agent sandbox permissions. They share the
provider credential scrub, with only their owning `DSH_SESSION_ID` explicitly
added. Output and user keystrokes never enter the model transcript or tool
schemas. Model sandbox changes invalidate only model runtimes.

Identities are scoped to one Session and Host lifetime. Creation is idempotent,
pending and retained identities each consume one quota slot, and a closed
identity cannot be recreated. Closing a Session fences its owner permanently.
The latest attachment controls input and resizing; older attachments continue
receiving output without regaining input when the newer one detaches. Refused
or cancelled input is never replayed. Detachment preserves the native shell,
variables, working directory, background jobs and emulated screen. Exit retains
the screen and exit status until explicit close, without spawning a replacement.

One PTY reader feeds an isolated VT emulator. The local TUI renders safe cell
snapshots and SGR styles; PTY clipboard, title, OSC and unrelated screen-control
sequences are never written to the user's terminal. Supported keyboard/mouse
input modes and cursor coordinates are reproduced from validated emulator state.
Slow followers fail explicitly and can reattach for a fresh screen. A writable
renderer can return terminal protocol replies through the same provider input
boundary; an unattached screen drains queries without inventing input.

Window holds are independent of attachments. The CLI keeps each created terminal
held for its App lifetime, including pane collapse and Session switching. Without
a hold, the official idle policy requires continuous confirmed prompt evidence,
stable revisions and complete owned-process observations. Unknown activity,
input waiting, stopped/background jobs and observation gaps protect the terminal.
Only actual provider writes invalidate private Bash/Zsh prompt evidence;
metadata and hold changes reset the idle interval. Failed cleanup retains the
identity and quota for retry. Owner/Host disposal stops automatic scheduling but
preserves provider readers when cleanup fails, allowing disposal to be retried.

`user_terminal` settings map the official controller deployment limits: defaults
are 8 terminals per Session, 500 columns, 200 rows, 1000 scrollback rows, 2 MiB
per-follower/input buffering, 65536 input bytes, 1000 ms disposal grace, 7200000 ms
unattended timeout, 30000 ms activity polling and 60000 ms cleanup retry. Zero
scrollback and unattended timeout are supported. Shell selection is separate
Client preference state in private `terminal-preferences.json`; unavailable
storage or a disappeared preferred shell falls back without blocking launch.

Current native providers support macOS and Linux. Process cleanup uses captured
PID/start identities; Linux also adopts session members, while macOS follows the
observable descendant tree. It does not claim kernel cgroup containment of
processes that deliberately escape that observed range. Font and base theme
come from the user's terminal. Browser tab layout, browser-only floating/dock
presentation, application OSC color overrides, and graphical font/contrast
controls are not reproduced by the native cell renderer. The TUI close action
currently waits for provider cleanup. Processes are never restored after Host
restart; reattaching an existing process never selects a different shell.
