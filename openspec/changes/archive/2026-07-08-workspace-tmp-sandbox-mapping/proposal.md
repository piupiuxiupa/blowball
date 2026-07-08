## Why

Agents sometimes write temporary files to `/tmp` inside the sandboxed `bash`/`python` tools, but `/tmp` is currently a private `tmpfs` that is discarded after each execution. Meanwhile, the `xizhi_*` file tools enforce strictly relative workspace paths, and any mention of the workspace root in the system prompt tempts the model to prepend it, producing nested paths that fail validation. We need a coherent path convention that models can follow, with engineering fallbacks so mistakes are recoverable.

## What Changes

- Add a **workspace path convention** to all agent base prompts in `config.yaml`:
  - `xizhi_*` paths must be relative to the workspace root.
  - The `bash`/`python` sandbox runs with `/workspace` as working directory.
  - The sandbox's `/tmp` is mapped to the workspace's `./tmp/` directory, so files written to `/tmp` persist at `tmp/` and can be read with `xizhi_read_file`.
- Change the bubblewrap sandbox setup so `/tmp` is a **bind mount** of `workspace/tmp/` instead of a fresh `tmpfs`.
- Ensure `workspace/tmp/` exists before each sandboxed execution.
- Improve `xizhi_*` error messages to include relative-path examples, helping the model self-correct when it uses absolute paths or `/tmp/...`.
- Update executor tool tests that assert the old `--tmpfs /tmp` argument.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `executor-tools`: The `bash`/`python` sandbox SHALL map `/tmp` inside the sandbox to the user's `workspace/tmp/` directory so that temporary files persist across executions and are reachable via `xizhi_*` tools.
- `xizhi-tools`: Path validation errors SHALL include guidance to use relative paths, e.g. `tmp/foo.txt` or `src/main.go`, when the model supplies an absolute path or attempts traversal.

## Impact

- `config.yaml` agent `system_prompt` fields.
- `internal/tool/executor/bwrap.go` and `internal/tool/executor/runner.go`.
- `internal/tool/xizhi/validate.go` error messages.
- `internal/tool/executor/bwrap_test.go` expected bwrap arguments.
- No API or database schema changes.
- No breaking changes for end users.
