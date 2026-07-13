## Why

Operators need to provide a set of CLI binaries (e.g. `node`, custom CLIs, data-fetch tools) that agents can invoke from the sandboxed `bash`/`python`/`pip_install` tools. Some of these tools **hardcode** `$HOME/.local/bin` as their lookup path, so the directory must resolve to a real, populated location *inside the bubblewrap sandbox* and be on `PATH`. Today the sandbox has no mounted home at all (`$HOME` leaks from the host env but points at an unmounted path), so such tools fail. This change adds an operator-managed `{data-dir}/tools` directory that is mounted read-only at `$HOME/.local/bin` and placed on `PATH`, with a real writable `$HOME` scaffold so commands that also write to `$HOME` keep working.

## What Changes

- Add a fourth runtime subdir `{data-dir}/tools`, created at startup alongside `data`/`logs`/`skills` (derived from the `-d`/`--data-dir` root).
- Cover `{data-dir}/tools` with **read-only** Landlock (defense-in-depth alongside the bwrap `--ro-bind`). This introduces a read-only-dir concept in the Landlock restriction, which today is write-only for the runtime subdirs.
- Inside the `bash`/`python`/`pip_install` bwrap sandbox:
  - Establish a real, writable, ephemeral `$HOME` at a fixed synthetic path via `--tmpfs`, so commands that cache/config under `$HOME` work (today they fail because `$HOME` points into the void).
  - Bind `{data-dir}/tools` read-only onto `$HOME/.local/bin` (order: tmpfs first, then the ro-bind on the subpath).
  - Force `HOME=<synthetic path>` in the sandbox env, overriding any host `HOME` that `allowed_env_patterns` would otherwise leak.
  - Prepend `$HOME/.local/bin` to `PATH` so the operator's tools are invocable by bare name and take precedence over host `/usr/bin`.
- Thread the new `toolsDir` through `serve.go` → `executor.NewTools` → `buildBwrapArgs` (same pattern already used for `globalSkillsDir`).

No **BREAKING** changes to public HTTP API or config schema. The only behavioral shift is inside the sandbox: `$HOME` becomes a real writable synthetic path and gains `/.local/bin` on `PATH`, which is strictly more functional than today (today `$HOME` writes fail). Operators who currently rely on the *value* of `$HOME` inside the sandbox (unlikely, given it pointed at an unmounted host path) should take note.

## Capabilities

### New Capabilities
<!-- None. All changes extend existing capabilities. -->

### Modified Capabilities
- `api-server`: The "Runtime data root" requirement now derives a fourth subdir `{data-dir}/tools` and creates it at startup (alongside `data`, `logs`, `skills`).
- `xizhi-tools`: The "Landlock process-level restriction" requirement additionally covers `{data-dir}/tools` as **read-only**, introducing a read-only directory class alongside the existing read-write runtime subdirs.
- `executor-tools`: The sandbox environment now provides a real writable `$HOME`, mounts operator tools read-only at `$HOME/.local/bin`, forces `HOME` in the sandbox env, and prepends `$HOME/.local/bin` to `PATH`.

## Impact

- **Code**:
  - `cmd/blowball/serve.go` — derive `toolsDir`, `MkdirAll`, pass to Landlock (RO) and `executor.NewTools`, log in the runtime-layout line.
  - `internal/tool/xizhi/landlock_linux.go` + `landlock_other.go` — split the restriction into read-write dirs (data/logs/skills) and read-only dirs (tools); update `ApplyLandlock` signature/callers.
  - `internal/tool/executor/executor.go` — add `toolsDir` field + `NewTools` parameter.
  - `internal/tool/executor/bwrap.go` — add `toolsDir` arg, emit `--tmpfs` HOME + `--ro-bind` of tools, force `HOME`, prepend `PATH`.
  - `internal/tool/executor/runner.go` — thread `t.toolsDir` into `buildBwrapArgs`.
  - Tests: `internal/tool/xizhi/landlock_rotation_test.go`, `internal/tool/executor/bwrap_test.go`, `test/integration/executor_test.go`.
- **Config**: No new required fields. `{data-dir}/tools` is fixed (mirrors `data`/`logs`/`skills`); an empty tools dir is harmless.
- **Docs**: `CLAUDE.md` (runtime layout, sandbox, landlock sections) and `config.example.yaml` (note the new dir).
- **Platforms**: Sandbox/Landlock behavior is Linux-only; macOS/Windows remain no-ops as today.
- **Known limitation (out of scope)**: tools that resolve the home directory via `getpwuid(getuid())` instead of the `HOME` env var will still see the host uid's passwd entry; a synthetic `/etc/passwd` override is deferred to a future change if a real tool proves to need it.
