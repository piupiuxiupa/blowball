## 1. Context plumbing

- [x] 1.1 Add a context-first full-parameter grep entry point that preserves the existing background-context `GrepSearch` wrapper for legacy callers.
- [x] 1.2 Switch the registered `xizhi_grep` callback to the context-first entry point so argument decoding and search execution use the registry context.
- [x] 1.3 Add a focused registration test proving that a pre-cancelled or short-deadline registry context reaches the actual `xizhi_grep` implementation instead of being replaced with `context.Background()`.

## 2. Cancellable search engines

- [x] 2.1 Carry the dispatcher context through `grepRun`, engine selection, and both engine `search` implementations.
- [x] 2.2 Keep ripgrep execution cancellable with `exec.CommandContext`, including its stdout-read and `Wait` failure path, while preserving the 10,000-match collection kill.
- [x] 2.3 Refactor pure-Go text reading into a buffered, context-checking line reader that preserves binary sniffing, CRLF handling, and the existing long-line skip behavior.
- [x] 2.4 Propagate cancellation errors from per-file scanning instead of reporting a cancelled search as an empty or silently skipped file.
- [x] 2.5 Make `attachContext` context-aware and stop before reading additional matching files or lines after cancellation.
- [x] 2.6 Add deterministic cancellation tests for the pure-Go engine, context-line assembly, and ripgrep execution; conditionally skip the ripgrep test when `rg` is unavailable.

## 3. Traversal policy

- [x] 3.1 Give the grep engine enough workspace-root context to identify the workspace-level `.blowball` namespace independently of the requested search root.
- [x] 3.2 Exclude `.blowball` and its descendants from pure-Go workspace-root recursive searches even when `include_hidden` is true.
- [x] 3.3 Apply the equivalent workspace-root-only exclusion to ripgrep without changing nested `.blowball` directories' ordinary hidden-entry semantics.
- [x] 3.4 Add tests for explicit `.blowball` rejection, root recursion exclusion with `include_hidden=true`, and ordinary hidden entries remaining searchable.
- [x] 3.5 Add or retain engine-parity tests for hidden descendants, explicit hidden single-file targets, and leading-8-KiB NUL binary skipping.

## 4. Validation and documentation

- [x] 4.1 Run `gofmt` on all changed Go files.
- [x] 4.2 Run `go test ./internal/tool/xizhi ./internal/tool`.
- [x] 4.3 Run `make test`.
- [x] 4.4 Run `make lint`.
- [x] 4.5 Confirm no tool JSON schema, OpenAPI contract, migration, or configuration-format change is needed.
