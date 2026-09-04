## Why

`tools.timeouts.xizhi_grep` is documented as a per-invocation execution budget, but the registered `xizhi_grep` callback discards the registry's deadline context and restarts the search with `context.Background()`. A production scan configured for 30 seconds can therefore block a turn for tens of minutes. The same investigation also found that recursive searches with `include_hidden=true` can enter the reserved `.blowball` workspace namespace when the requested root is `"."`.

## What Changes

- Propagate the registry-provided context through the full `xizhi_grep` execution path, including engine selection, validation, the ripgrep subprocess, the pure-Go walker, and context-line reads.
- Make `xizhi_grep` respect the configured total wall-clock timeout: the budget starts when the registry dispatches the tool and is not an idle/no-output timeout.
- Ensure the pure-Go fallback and post-search context attachment can stop cooperative work after cancellation instead of reading unbounded text files without checking the deadline.
- Preserve existing binary and hidden-entry behavior: binary files are silently skipped; hidden descendants are excluded unless `include_hidden=true`; an explicitly requested hidden file remains searchable.
- **BREAKING** for a narrow existing behavior: recursive searches with `include_hidden=true` SHALL NOT descend into the reserved `.blowball` namespace, even when the requested root is `"."`.
- Add regression coverage for context propagation, timeout cancellation, large-file context reads, reserved-namespace traversal, binary skipping, and hidden-entry semantics.

## Capabilities

### New Capabilities

<!-- None. -->

### Modified Capabilities

- `tool-execution-timeout`: Clarify and enforce that registry tools must consume the dispatcher-provided deadline context; a configured `xizhi_grep` timeout must bound the real registered tool, including its search engines and follow-up reads.
- `xizhi-grep-files`: Define cancellation behavior for directory and single-file searches, and extend reserved-namespace exclusion to entries discovered during recursive traversal.

## Impact

- Affected code: `internal/tool/xizhi/register.go`, `internal/tool/xizhi/grep_engine.go`, `internal/tool/xizhi/grep.go`, `internal/tool/xizhi/grep_rg.go`, and focused tests in `internal/tool/xizhi`.
- No HTTP API, JSON tool schema, configuration key, migration, or dependency changes are planned.
- Operators who already configure `tools.timeouts.xizhi_grep` will see that configuration take effect; pathological searches may now return the normal timeout failure envelope instead of blocking a turn.
