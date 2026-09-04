## Context

The per-tool timeout feature derives a deadline context in `tool.Registry.Call` and passes it to each registered `Execute` callback. The `xizhi_grep` callback currently decodes its arguments but calls `GrepSearch` without that context; `GrepSearch` then calls `grepRun(context.Background(), ...)`. Consequently, the ripgrep engine starts with `exec.CommandContext(context.Background(), ...)`, and the pure-Go engine checks an already-unbounded context. Post-search context-line assembly also reads whole text files without a context argument.

The search path also has two relevant traversal rules. Hidden descendants are skipped unless `include_hidden=true`, while an explicitly requested target is exempt from hidden filtering. Binary detection examines the leading 8 KiB for a NUL byte and skips affected files silently. Reserved `.blowball` paths are rejected when they appear in the requested path, but a workspace-root search with `include_hidden=true` can currently discover the namespace during traversal.

## Goals / Non-Goals

**Goals:**

- Make an already-configured `tools.timeouts.xizhi_grep` budget bind the actual registered tool invocation.
- Preserve the existing public `GrepSearch` behavior for legacy callers while providing a context-aware path for registry dispatch.
- Propagate cancellation through ripgrep execution, pure-Go traversal, text scanning, and context-line assembly.
- Keep hidden and binary filtering deterministic across both search engines.
- Prevent recursive workspace-root searches from entering the reserved `.blowball` namespace.
- Add regressions that fail if a future registration path drops the dispatcher context again.

**Non-Goals:**

- Do not introduce a new timeout configuration key or change the flat `tools.timeouts` map.
- Do not change the tool's JSON schema, result envelope, pagination model, or match shape.
- Do not change `xizhi_read_file`, `xizhi_find`, or the generic registry timeout configuration semantics.
- Do not attempt to make synchronous reads from an unresponsive network filesystem interruptible in all possible kernel states; cancellation is checked between normal buffered file-I/O operations.
- Do not add a process-wide goroutine watchdog that returns at the deadline while leaving arbitrary leaked tool goroutines running.

## Decisions

### D1: Add a context-aware grep entry point and use it from registration

Keep `GrepSearch` as the existing background-context compatibility entry, and add a context-first entry (for example, `GrepSearchCtx`) that delegates to `grepRun(ctx, ...)`. The registered `xizhi_grep` callback will decode arguments and call that context-first entry.

This avoids breaking existing internal callers and tests while making the registry path explicit. Passing `ctx` into the existing `GrepSearch` signature would be a broader API change and would not make the registry contract clearer.

### D2: Make cancellation cooperative but thorough

The deadline remains a total execution budget that begins at registry dispatch. It is not an idle/no-output timeout and does not reset when output is produced.

For ripgrep, the real context must be supplied to `exec.CommandContext`. When the deadline fires, Go kills the subprocess; the existing stdout scanner and `Wait` path then unblock and return `context.DeadlineExceeded`. Collection capping continues to kill ripgrep early as before.

For the pure-Go engine, cancellation must be checked before walking each entry and throughout text-file reads. `readTextLines` will move from an opaque whole-file `bufio.Scanner` operation to a buffered, line-oriented read loop that:

- checks `ctx.Err()` before I/O and between reads/lines;
- preserves the leading 8 KiB NUL-byte binary sniff;
- preserves the existing maximum-line behavior; and
- returns cancellation as an error rather than treating it as a silently skipped file.

Context-line assembly receives the same context and stops before reading another matching file or another line after cancellation. This is intentionally cooperative: it avoids a generic dispatcher goroutine leak while ensuring all normal local-file work observes the deadline.

### D3: Preserve engine parity while filtering during traversal

The ripgrep and pure-Go engines must continue producing the same fields for the same searchable inputs. Hidden-entry and binary behavior remain:

- a name beginning with `.` is hidden;
- hidden descendants are pruned/skipped by default and included with `include_hidden=true`;
- an explicitly requested single-file target remains searchable even when its basename is hidden;
- a NUL byte in the leading 8 KiB marks a file binary, and binary files produce no matches and no error.

Tests exercised through both engines (with ripgrep tests skipped when `rg` is unavailable) guard these parity rules.

### D4: Exclude the reserved root namespace during recursive discovery

`grepInput` will carry enough information to distinguish the workspace root from a nested search root. Directory traversal from the workspace root skips `.blowball` and all descendants even when `include_hidden=true`. Ripgrep receives the equivalent exclusion only for a workspace-root directory search. An explicit `.blowball/...` target continues to fail `validatePath` before an engine starts.

Only the workspace-level `.blowball` namespace is reserved; a nested directory that happens to be named `.blowball` is not silently assigned application-state semantics.

## Risks / Trade-offs

- [A blocked filesystem syscall can outlive a cooperative deadline] → The implementation checks cancellation around buffered reads and uses process killing for ripgrep. A generic goroutine watchdog is deliberately rejected because it can hide leaked file reads and subprocesses.
- [Context-aware line reading may change edge-case behavior] → Preserve the 8 KiB binary window, CRLF handling, and maximum-line skip behavior with focused unit tests for empty, CRLF, long-line, binary, and cancellation cases.
- [Excluding `.blowball` changes a narrow historical result] → The namespace is already reserved for direct paths, so recursive discovery is a policy leak rather than a supported capability. The behavior is called out in the proposal.
- [Ripgrep availability differs across deployments] → Keep the pure-Go fallback and test both engines; conditionally skip ripgrep parity tests only when the binary is absent.

## Migration Plan

1. Introduce the context-first search entry and switch the registered callback to it.
2. Thread cancellation through both engines and shared text/context helpers.
3. Add reserved-root traversal filtering.
4. Add focused unit tests, then run the xizhi and tool package tests followed by `make test` and `make lint`.

Rollback is a normal code rollback: no configuration, schema, persistence, or deployment migration is introduced.

## Open Questions

None.
