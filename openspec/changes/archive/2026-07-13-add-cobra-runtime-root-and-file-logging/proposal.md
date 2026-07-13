## Why

The server hard-codes the two operational knobs deployments most need to control: the config path is pinned to `config.yaml`, and all on-disk state is rooted at CWD-relative literals (`DataDir = "data"`, global skills at `"skills"`). There is also no persistent log file at all — zap writes only to stderr, so logs are lost when the process or container exits. As blowball moves toward containerized/production deployment, operators need to (a) point the binary at an arbitrary config file, (b) gather all runtime state — data, logs, and global skills — under a single root directory that can be a mounted volume, and (c) keep structured logs on disk with rotation. Introducing cobra also gives us a clean home to unify the two existing binaries (`cmd/server`, `cmd/seed`) into one tool with subcommands.

## What Changes

- **Introduce cobra** as a unified `blowball` binary exposing `serve` and `seed` subcommands, with persistent flags `-f`/`--config` (config file path) and `-d`/`--data-dir` (runtime root) shared by both. **BREAKING**: the standalone `bin/seed` binary is replaced by `blowball seed`; the Makefile `build` target and any scripting must be updated.
- **Runtime data root (`-d`)**: a single directory containing `data/`, `logs/`, and `skills/`. Defaults to the current working directory (`.`), so existing `./data` and `./skills` resolve unchanged and a new `./logs` is added — fully backward compatible when `-d` is omitted.
- **File logging with rotation**: extend the zap logger to tee output to the console *and* a file under `{data-dir}/logs/`, rotated by size/age/backups via lumberjack. The already-declared but currently ignored `logging.format` field becomes effective (`json` | `console`).
- **Bootstrap reorder**: parse CLI flags → load config (`-f`) → create `{data-dir}/logs` → init logger (writing into it) → init stores under `{data-dir}/data` → apply landlock to the runtime root.
- **Widen the Linux landlock target** from `data/` to the `-d` runtime root, so lumberjack's post-rotation file reopens stay inside the sandbox. *Security-relevant*: the sandbox grows from `data/` to the whole runtime root, so production deployments should point `-d` at a dedicated directory rather than the repo root.
- **Relocate the global skills directory** from the literal `./skills` to `{data-dir}/skills`; the loader and executor resolve it from `-d`. (`/skills/` is already gitignored, so global skills are operator-provided regardless — this changes where they live, not how they ship.)

## Capabilities

### New Capabilities
<!-- none — all changes extend the existing api-server capability -->

### Modified Capabilities
- `api-server`: add a command-line interface (cobra `serve`/`seed` subcommands with `-f`/`-d` persistent flags and backward-compatible defaults), a runtime data root from which `data/`, `logs/`, and `skills/` are derived, a configurable config-file path, and on-disk log persistence with size/age/count rotation. Widen the Linux landlock sandbox target from `data/` to the runtime root.

## Impact

- **Code**: `cmd/server/main.go` (cobra wiring, bootstrap reorder, path derivation, landlock target), new unified entrypoint (e.g. `cmd/blowball/`), `cmd/seed` folded into a subcommand, `internal/pkg/logger/zap.go` (file sink + lumberjack + honored `format`), `internal/config/config.go` (optional `logging.output` / `logging.file.*` keys), `internal/tool/xizhi` landlock call site, `Makefile` (build-target rename).
- **Dependencies**: add `github.com/spf13/cobra` and `gopkg.in/natefinch/lumberjack.v2`.
- **Config**: new optional `logging.output` (default `[stderr, file]`) and `logging.file.{max_size_mb,max_backups,max_age_days,compress}` keys; `logging.format` (`json`|`console`) becomes effective.
- **Operations**: `-d` enables a single mounted volume for all runtime state; `bin/seed -username …` becomes `blowball seed -username …`. CLAUDE.md and README updated for the new flags and directory layout.
- **Security**: landlock scope widened from `data/` to the runtime root — see design.md for the tradeoff and the recommendation to use a dedicated `-d` in production.
