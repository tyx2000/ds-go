# Sent user text masking

`SecretSpans`, `FindSecretSpan`, and `MaskUserText` port the pure credential rule
from DeepSeek Harness commit `687f282e8625c6b8d11ff58211a90524be2e4317`.
They recognize the published provider/token prefixes, JWTs, private-key blocks,
and credential-bearing address passwords. Arbitrary passwords and `name=value`
assignments remain plain unless their value independently has a recognized format.

All exported offsets are **UTF-8 bytes**, with an exclusive end. Internal scans
use UTF-16 code units to preserve the official bounds and query behavior.
Masks preserve the first eight ASCII characters of prefixed tokens, hide address
passwords completely, keep multiline private-key headers/footers, and preserve
CR/LF. Each hidden Unicode code point becomes one `*`. Go user text is valid
UTF-8; JavaScript strings containing isolated surrogate code units have no
byte-identical Go string representation.

Use these functions only for sent user text displayed in the terminal. Keep the
original message for the model, durable history, copy, and export. Do not replace
the original with `MaskUserText` output or apply this rule to tool/model output.

References and credentials share a nearest-start selection. A reference that
begins earlier keeps its rendering; at the same start the secret rule wins
(official rule order `0`). After consuming a reference, call `FindSecretSpan`
again from its end. Simply deleting overlapping entries from `SecretSpans` can
miss an address password whose earlier `://` was consumed by the reference.

Sources are the pinned `packages/client/ui-secret-mask/src/client/` modules
`credential-formats.ts`, `credential-masks.ts`, `credential-scanner.ts`,
`format-tables.ts`, and `secret-rule.ts`. The Go scanner ports literal overlap,
deferred address ordering, earliest-candidate masks, transitive overlap, run
memoization, and the ASCII/JavaScript whitespace classes without broad regexes.

The independent golden fixture generator imports only those pure TypeScript
modules using Node 24. After downloading the pinned modules to a directory:

```sh
node internal/tui/secrets_upstream_generate.mjs /private/tmp/dsgo-secret-mask-upstream > /private/tmp/dsgo-secrets-golden.json
DSGO_SECRET_GOLDEN=/private/tmp/dsgo-secrets-golden.json go test ./internal/tui -run '^TestSecret' -count=1
```

Its JSON records source-file checksums, published-format boundaries, Unicode
address queries, overlapping ranges, and deterministic mixed-format cases.
