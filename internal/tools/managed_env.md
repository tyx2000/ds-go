# Managed shell environment

Fresh `standard_bash` calls rebuild the official `DSH_*` facts for each execution: `DSH_HOME`, `DSH_SHELL=1`, and the calling session's `DSH_SESSION_ID` when an owner exists. A real launcher profile can additionally supply `DSH_PROFILE` and `DSH_PROFILE_DIR`; the terminal port has no launcher profile and leaves those absent. The application's resolved state root explicitly supplies `DSH_HOME`. Bare library calls resolve explicit home, ambient nonblank `DSH_HOME`, then `~/.dsh`, using host process cwd for relative paths.

A fresh command's `workdir` changes only that process, and a background command retains its launch snapshot. Persistent model shells, PTY tools, PTC Node execution and Host user terminals are outside the current product scope; their former identity facts are not injected.

Every generic child starts with credential-shaped names and ambient `DSH_*` removed, ignoring case. The port also scrubs its internal `DSGO_*` namespace. Ordinary `PATH`, `HOME`, locale and proxy variables survive. Trusted explicit layers merge afterward. Fresh bash keeps ordinary `BASH_ENV`/`ENV` startup behavior, matching the official executor.

`managed-env-corpus.json` and `check-managed-env.mjs` compare built-in identity facts against the fixed source without starting a Harness or installing plugins. The source registry has an extensible contributor API, but this port does not expose plugin loading or arbitrary contributor configuration. A Web-only `DSH_WEB_URL` is absent because this terminal process does not advertise a GUI URL.

The fixed subprocess scrub also applies an installed proxy policy's child overlay (resolved fallbacks, loopback bypasses and Node's proxy opt-in). The current port preserves exported proxy names but has not yet reproduced that policy installation/overlay; this remains a confirmed difference rather than an inferred environment fact.

Sources at fixed commit `687f282e8625c6b8d11ff58211a90524be2e4317`:

- `packages/subprocess/subprocess/src/index.ts`: `scrubbedParentEnv`.
- `packages/subprocess/subprocess-local/src/spawn.ts`: explicit environment layering.
- `packages/shell/shell-env/src/index.ts`: registry built-ins and per-execution collection.
- `packages/shell/bash-local/src/index.ts`: ordinary env, managed facts and terminal overrides.
- `packages/shell/tool-bash/src/index.ts`: session cwd, workdir and snapshot ownership.
- `packages/util/home-paths/src/index.ts`: home resolution.
- `packages/util/http-proxy/src/install.ts`: remaining child proxy overlay.
