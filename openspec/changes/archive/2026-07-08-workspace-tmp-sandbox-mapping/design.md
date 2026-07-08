## Context

The workspace file tools (`xizhi_*`) require strictly relative paths scoped to the user's workspace. Earlier attempts to include the workspace root path in the system prompt caused the model to nest it (e.g. `/workspace/.../workspace/...`), which fails validation. The workspace path has therefore been removed from the rendered environment section.

At the same time, agents frequently use `/tmp` inside the sandboxed `bash` and `python` tools for intermediate files. The current sandbox mounts a fresh `tmpfs` at `/tmp`, so those files disappear when the sandbox exits and cannot be read back with `xizhi_read_file`.

This change establishes a single, model-facing path convention and backs it with sandbox-level behavior and tool-level error guidance.

## Goals / Non-Goals

**Goals:**
- Define a clear prompt-level rule: all `xizhi_*` paths are relative to the workspace root; temporary files go under `./tmp/`.
- Make the sandbox's `/tmp` actually map to `workspace/tmp/` so that model-written temp files persist and are reachable.
- Help the model self-correct by including relative-path examples in `xizhi_*` validation errors.
- Update executor tests that assert the old `--tmpfs /tmp` bwrap argument.

**Non-Goals:**
- Re-introducing the absolute workspace path into the rendered `# Environment` section.
- Adding a new `xizhi_*` tool for temp-file management.
- Changing landlock, network isolation, audit logging, or dangerous-command detection.
- Solving concurrent-write collisions inside `workspace/tmp/` beyond prompt guidance.

## Decisions

### 1. Bind `workspace/tmp/` to `/tmp` instead of using `--tmpfs /tmp`
- **Rationale**: A bind mount makes `/tmp` inside the sandbox the same directory as `workspace/tmp/` on the host. Files written by `bash`/`python` survive the sandbox exit and can be read via `xizhi_read_file tmp/...`.
- **Alternative considered**: Keep `--tmpfs /tmp` and only set `TMPDIR=/workspace/.tmp`. Rejected because explicit `/tmp/foo` writes would still be lost.

### 2. Create `workspace/tmp/` on demand in `runner.go`
- **Rationale**: `bwrap --bind` requires the source directory to exist. Creating it in `run()` keeps the mount logic in one place and avoids a startup-time directory creation step.
- **Alternative considered**: Create it at user/workspace creation time. Rejected because existing workspaces would lack the directory.

### 3. Centralize the workspace path convention in `RenderSystemPrompt`
- **Rationale**: The convention applies to every agent, so rendering it once in `RenderSystemPrompt` avoids duplicating the same block across every agent base prompt and guarantees a consistent message. It also makes the rule testable as part of the rendered prompt.
- **Alternative considered**: Keep the rule in `config.yaml` base prompts. Rejected because it leads to repetitive configuration and makes it easy for individual agent prompts to drift out of sync.

### 4. Add relative-path examples to validation errors
- **Rationale**: Models often recover from a failed tool call when the error message tells them exactly how to fix the argument.
- **Alternative considered**: Retry/re-write paths automatically inside the tool. Rejected because silently rewriting absolute paths hides model confusion and could create security ambiguity around `/tmp`.

## Risks / Trade-offs

| Risk | Mitigation |
|------|------------|
| Concurrent `bash`/`python` invocations share `workspace/tmp/` and may overwrite each other's files. | Prompt instructs the model to use unique names; collisions are accepted as a known limitation of a single shared temp space. |
| Model still passes `tmp/foo.txt` to `xizhi_*` and expects it under `/tmp`. | Prompt explicitly says "files written to `/tmp` are read via `xizhi_read_file` at `tmp/...`". |
| Tests asserting `--tmpfs /tmp` fail after the bind change. | Update `internal/tool/executor/bwrap_test.go` to expect `--bind <workspaceTmp> /tmp`. |
| `workspace/tmp/` could accumulate junk over time. | Out of scope for this change; user can delete via `xizhi_*` or workspace UI. |

## Migration Plan

No migration needed. Existing workspaces will gain a `tmp/` directory on the next `bash`/`python` invocation.

## Open Questions

None.
