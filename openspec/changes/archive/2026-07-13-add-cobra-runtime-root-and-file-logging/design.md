## Context

Today every operational path in `cmd/server/main.go` is a hard-coded literal: the config is loaded from `"config.yaml"`, on-disk state is rooted at `const DataDir = "data"`, and global skills are read from the literal `"skills"` (passed to both `skill.NewLoader` and `executor.NewTools`). Logging goes through `logger.Init(level)`, which builds a production zap config with JSON encoding and no `OutputPaths` override — so it writes to **stderr only**, with no persistence and no rotation. `LoggingConfig.Format` is declared but `Init` ignores it. Two separate binaries exist: `cmd/server` (no flags) and `cmd/seed` (stdlib `flag`, `-config`). The `/skills/` and `/data/` directories are gitignored, so they are operator-/runtime-populated, not shipped artifacts.

This change introduces cobra and a configurable runtime root so a deployment can gather config, data, logs, and skills under operator-chosen paths — most importantly placing all mutable state behind a single `-d` volume mount and keeping logs on disk with rotation.

## Goals / Non-Goals

**Goals:**
- Unify `cmd/server` and `cmd/seed` into one cobra binary (`blowball serve`, `blowball seed`) with persistent `-f`/`--config` and `-d`/`--data-dir` flags.
- Derive `data/`, `logs/`, and `skills/` from a single runtime root (`-d`), defaulting to the current working directory so existing layouts resolve unchanged.
- Persist structured logs to `{data-dir}/logs/` with size/age/count rotation, teed with the console. Honor `logging.format` (`json` | `console`).
- Keep the change backward compatible: `-d .` (the default) reproduces today's `./data` + `./skills` and merely adds `./logs`.
- Keep landlock functional on Linux after the move (rotation reopens files post-sandbox).

**Non-Goals:**
- No data migration of existing files — old `./data` and `./skills` keep working in place when `-d` is omitted.
- No new subcommands beyond `serve` and `seed` (e.g. `migrate`, `version`) — left for a future change, though cobra makes adding them trivial.
- No per-subdir config overrides (`storage.data_dir`, etc.); `-d` is the single lever. Can be added later if a deployment needs logs on a separate volume.
- No change to the executor's *sandbox* behavior (mount points `/skills/global`, `/workspace`, etc. are unchanged); only the host-side source paths move under `-d`.
- No remote/log-shipping sinks (syslog, fluentd). File + console only.

## Decisions

### D1: Unify into one cobra binary with `serve` / `seed` subcommands
**Choice:** Single `blowball` binary; `serve` replaces the server entrypoint, `seed` absorbs `cmd/seed`. `-f` and `-d` are persistent flags on the root, inherited by both subcommands.
**Rationale:** The user explicitly asked to introduce cobra. stdlib `flag` already handles `-f`/`-d` (seed proves it), so cobra only earns its keep if we use its command tree. Unifying seed gives shared persistent flags, consistent help, and headroom for future commands. Folding seed in is a small extra scope (seed already uses `flag` and `config.Load`).
**Alternatives considered:**
- *Flags only, no subcommand* (`blowball -f … -d …`): lightest, but under-uses cobra and leaves seed as a second binary.
- *`serve` subcommand only, keep seed separate*: matches the literal ask but forfeits the shared-flags / single-binary benefit.

### D2: `-d` is the sole derivation root; no per-subdir config overrides
**Choice:** `data` = `{d}/data`, `logs` = `{d}/logs`, `global skills` = `{d}/skills`. Fixed child names; no YAML overrides.
**Rationale:** Matches the user's stated intent ("用于存放 data, logs, skills"). One knob, one volume mount. Backward compatible because `-d` defaults to `.`.
**Alternatives considered:**
- *Config-overridable subpaths* (`storage.data_dir`, `logging.file.dir`, `skills.dir`): more flexible (logs on a separate volume) but adds YAML surface area and precedence rules we don't need yet. Defer.

### D3: `-d` defaults to the current working directory (`.`)
**Choice:** `--data-dir` default is `.` (CWD), not `"data"` and not a system path.
**Rationale:** With `-d .`, the three children resolve to `./data`, `./logs`, `./skills` — identical to today for data and skills, purely additive for logs. Defaulting to `"data"` would nest `data/data`; a system path would surprise dev workflows.
**Consequence:** `const DataDir = "data"` becomes `filepath.Join(dataDir, "data")`.

### D4: Logger tees console + file, rotated by lumberjack
**Choice:** Build two zap cores — console (stderr) and file — and tee them (`zapcore.NewTee`). The file core writes through `gopkg.in/natefinch/lumberjack.v2` for size/age/backup rotation. `logging.output` (`[stderr|stdout, file]`, default `[stderr, file]`) selects sinks; `logging.format` (`json` default | `console`) selects encoding for both cores.
**Rationale:** Console keeps `docker logs` / direct terminal output visible; the file gives persistence and bounded disk usage via rotation. lumberjack is the de-facto Go rotation library and integrates with zap as a `zapcore.WriteSyncer`.
**Alternatives considered:**
- *zap's built-in multi `OutputPaths`* (`["stderr", "/path"]`): simplest, but no rotation — unbounded growth. Rejected for a long-running server.
- *File-only*: loses container/console visibility. Rejected.
- *No rotation*: unbounded log file. Rejected.

### D5: Bootstrap order — flags before logger before stores
**Choice:**
```
0. cobra parse           → resolve -f, -d
1. config.Load(-f)
2. os.MkdirAll({d}/logs)  → ensure log dir exists before logger opens it
3. logger.Init(cfg.Logging, {d}/logs)   → writes to the right file from the first line
4. fs.New({d}/data); stores; xizhi.RegisterAll({d}/data, …)
5. ApplyLandlock(<runtime root or subdirs>)   → after logger open, before serving
6. … services, orchestrator, handlers, server …
```
**Rationale:** `-d` is known at step 0, so the logger (step 3) can target `{d}/logs` immediately — no bootstrapping chicken-and-egg. The log dir is created (step 2) before the logger opens it (step 3). Landlock stays after logger init (matches today's order), so the initial file open is pre-sandbox; lumberjack's *rotation* reopens post-sandbox, which D6 handles.

### D6: Widen landlock from `data/` to the runtime root (preferred: the three subdirs)
**Choice:** On Linux, restrict the process to the runtime root. **Preferred implementation: landlock the specific subdirectories we touch — `{d}/data`, `{d}/logs`, `{d}/skills` — rather than the whole `{d}`**, keeping the sandbox as tight as today while still covering logs for rotation. If `go-landlock` multi-path usage proves awkward, fall back to landlocking `{d}` wholesale.
**Rationale:** lumberjack renames + reopens the log file on rotation; that reopen happens *after* landlock is applied. If `{d}/logs` is outside the sandbox, rotation silently fails. Landlocking the three subdirs (or the root) keeps logs reachable. Tight-per-subdir is preferred so a `{d}` that also contains unrelated files (e.g. the repo root in dev) does not get wholly exposed.
**Alternatives considered:**
- *Keep landlock at `data/` only*: rotation of `{d}/logs` breaks post-sandbox. Rejected.
- *Landlock `{d}` wholesale*: simplest, but exposes everything under the root — acceptable if `{d}` is dedicated, risky if it is the repo root. Used only as fallback.

### D7: Relocate global skills to `{d}/skills`
**Choice:** `skill.NewLoader("skills", …)` and the executor's global-skills argument resolve to `{d}/skills` instead of the literal `"skills"`.
**Rationale:** `/skills/` is gitignored — global skills are operator-provided, not shipped — so moving them under `-d` changes where they live, not how they ship. No spec pins the host path (`executor-tools` / `skill-directory-sandbox-access` refer to "the global skills directory" abstractly and mount it at `/skills/global` regardless), so no behavior changes at the sandbox boundary. Per-user skills already follow the data root (`data/{userID}/skills`).

### D8: Honor `logging.format`; fail fast on log-dir errors
**Choice:** Wire `logging.format` (`json` | `console`, default `json`) into the encoder. If file logging is enabled and `{d}/logs` cannot be created or the file cannot be opened, **fail fast** at startup (consistent with the existing `log.Fatal` style) rather than silently degrading — a server running without its configured persistent logging is a misconfiguration the operator should see immediately.

## Risks / Trade-offs

- **[Wider landlock scope when `-d` is the repo root]** → weakening defense-in-depth (a compromised server process could read config.yaml/source). *Mitigation:* default `-d` is `.` and landlock is a no-op on macOS (the dev platform); on Linux prefer per-subdir landlocking (D6); document that production should set `-d` to a dedicated directory.
- **[Breaking: `bin/seed` → `blowball seed`]** → any scripting/Makefile target invoking `bin/seed` breaks. *Mitigation:* update `Makefile` (`make build` → `bin/blowball`, `make run` → `serve`); document in README/CLAUDE.md; old invocation yields a clear cobra usage error.
- **[Log-dir not writable in a locked-down deployment]** → server refuses to start. *Mitigation:* documented as fail-fast (D8); operator fixes perms or sets `logging.output: [stderr]` to run console-only.
- **[lumberjack `panic`/`os.O_APPEND` interaction with landlock on rotation]** → *Mitigation:* D6 ensures the logs dir is inside the sandbox; covered by a Linux-only integration/unit check that triggers a rotation under landlock.
- **[Two binaries → one affects `go run ./cmd/...` dev workflows]** → *Mitigation:* README updated to `go run ./cmd/blowball serve`; Makefile is the supported path.

## Migration Plan

1. Add deps (`cobra`, `lumberjack.v2`); refactor `logger.Init` signature to accept logging config + log dir.
2. Create `cmd/blowball/` (cobra root + `serve`/`seed`); move server bootstrap into `serveRun`, seed logic into `seedRun`. Remove `cmd/server` and `cmd/seed` (or leave `cmd/server` as a thin deprecated shim for one release — *decision: remove, keep clean*).
3. Update `Makefile`: `make build` → `bin/blowball`; `make run` → `./bin/blowball serve`; add `make seed` convenience.
4. Replace the `DataDir`/`"skills"` literals with `{d}`-derived paths; widen landlock per D6.
5. Update `config.example.yaml` with `logging.output` / `logging.file.*` / `logging.format`.
6. Update CLAUDE.md (commands, dir layout, landlock note) and README.
- **Rollback:** revert the binary; no on-disk data migration is needed because `-d .` (default) reproduces the historical `./data` + `./skills` layout exactly — only the additive `./logs` dir appears. Existing data/skills are untouched.

## Open Questions

- **go-landlock multi-path:** confirm `xizhi.ApplyLandlock` (or its underlying `go-landlock` API) accepts multiple paths so we can landlock `{d}/data`, `{d}/logs`, `{d}/skills` individually (D6 preferred form). If not, fall back to `{d}` wholesale. Resolve at task 1 of implementation.
- **Console sink is `stderr` or `stdout`?** zap's production default is stderr; keep stderr for the console core so `2>` capture and existing behavior are preserved. (Lean: stderr.)
- **Should `-d` also move the config file?** No — config is an input, stays at `-f`, orthogonal to `-d`. (Decided: no; recorded here for clarity.)
- **Future `--log-level` / `--log-file` CLI overrides?** Out of scope; config-driven is enough for now.
