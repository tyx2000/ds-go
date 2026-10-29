# Workspace file candidates

`NewFiles(root, FileOptions{})` uses the fixed official file-reference search
defaults: at most 20 results and 50,000 indexed paths, with dependency/VCS/build
directories excluded. Nil exclusions select those defaults; an explicitly empty
slice disables them. No Git ignore rules are applied, and `lib` remains visible.

Empty queries and queries containing a slash list the selected directory live.
Bare fuzzy queries share one breadth-first index: the first query waits, and a
stale index answers immediately while one replacement builds. Cancelling a
caller only stops that caller's wait. Invalidation during a scan makes the
completed generation stale, and a failed replacement keeps the last good index
eligible for retry. Close aborts the shared traversal and suppresses publication.

Ranking preserves the official exact-name, name-prefix, name-substring, path and
subsequence tiers, the directory bonus, and UTF-16 length/lexical tie-breaking.
Global queries hide dot-path segments unless the query explicitly starts with a
dot; live directory queries apply the same policy to the current fragment.
Directory symlinks are not followed, nonregular entries are not offered, and
directory queries cannot escape the selected root. Discovery reads directory
entries only, never file contents.

`ActiveAtToken` uses rune cursor offsets for the terminal adapter while retaining
the official `@path` / `@"path with spaces` grammar. `FormatFileMention` rejects
control characters and double quotes, quotes whitespace, and leaves a selected
quoted directory open for further descent. The result stays ordinary prompt
text; selecting it does not attach or read a file.

Fixed source: [`file-reference-local/search.ts`](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/context/file-reference-local/src/search.ts),
[`file-reference/grammar.ts`](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/context/file-reference/src/grammar.ts).
