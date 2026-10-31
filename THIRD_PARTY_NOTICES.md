# Attribution

This project is an independent Go port of the official DeepSeek Harness user-facing functionality. It is not an official DeepSeek distribution.

- Upstream: [deepseek-ai/deepseek-harness](https://github.com/deepseek-ai/deepseek-harness/tree/687f282e8625c6b8d11ff58211a90524be2e4317), Copyright (c) 2026 DeepSeek, MIT. The pinned bundle and preset definitions under `docs/upstream/` are reproduced as migration references; upstream's complete license is preserved there. Protocol, authentication, namespace naming, and tool behavior are adapted with that attribution.
- `github.com/creack/pty` v1.1.24 (tests only), Copyright (c) 2011 Keith Rarick, MIT. License retained under `docs/licenses/pty.LICENSE`.
- `golang.org/x/net` v0.61.0, Copyright The Go Authors, BSD 3-Clause. License retained under `docs/licenses/x-net.LICENSE`.
- `golang.org/x/image` v0.47.0, Copyright The Go Authors, BSD 3-Clause. License retained under `docs/licenses/x-image.LICENSE`.
- `golang.org/x/text` v0.43.0, Copyright The Go Authors, BSD 3-Clause. License retained under `docs/licenses/x-text.LICENSE`.
- `github.com/santhosh-tekuri/jsonschema/v6` v6.0.3, Apache License 2.0. License retained under `docs/licenses/jsonschema.LICENSE`.
- MCP client protocol behavior is adapted from `@modelcontextprotocol/client` 2.2.0, TypeScript SDK source commit `dd22ba25ddbe7cd8822e1fe9c738a974996a28f6`. The SDK's complete licensing and transition notice (including Apache-2.0, retained MIT contributions, and documentation terms) is preserved in `internal/mcp/SDK_LICENSE`; package integrity and provenance are recorded in `internal/mcp/sdk-baseline.json`. The TypeScript SDK is not installed or shipped as a runtime dependency.

Bubblewrap and the operating system's sandbox utilities are external runtime dependencies on their respective platforms.
