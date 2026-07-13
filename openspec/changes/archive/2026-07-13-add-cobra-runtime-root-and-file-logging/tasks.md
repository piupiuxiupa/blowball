# Implementation Tasks

## 1. Dependencies & spike

- [x] 1.1 Add `github.com/spf13/cobra` and `gopkg.in/natefinch/lumberjack.v2` (`go get … && go mod tidy`).
- [x] 1.2 Spike: confirm `xizhi.ApplyLandlock` / underlying `go-landlock` API supports multiple paths. Decide D6 form — per-subdir (`{d}/data`,`{d}/logs`,`{d}/skills`) preferred, else whole `{d}`. Record outcome in code comment.

## 2. Logger refactor (`internal/pkg/logger`)

- [x] 2.1 Change `Init` signature to accept the logging config and a log-directory path (e.g. `Init(cfg config.LoggingConfig, logDir string)`); keep installing the package default via `logger.L()`.
- [x] 2.2 Build two zap cores and `zapcore.NewTee` them: a console core to **stderr** and a file core whose `WriteSyncer` is a `lumberjack.Logger` rooted at `{logDir}/blowball.log`. Skip the file core when `output` omits `file`; skip the console core when it omits console.
- [x] 2.3 Honor `logging.format` (`json` default | `console`) for both cores via the encoder config; keep ISO8601 timestamps and existing field set.
- [x] 2.4 Fail fast: if the file sink is enabled and `{logDir}` cannot be created or the file cannot be opened, return an error so startup aborts (do not silently degrade).
- [x] 2.5 Unit tests: json vs console encoding; tee writes to both sinks; `output` config selects sinks; a rotation is triggered when content exceeds `max_size_mb`.

## 3. Config (`internal/config`)

- [x] 3.1 Extend `LoggingConfig`: add `Output []string` (default `["stderr","file"]`) and `File struct { MaxSizeMB, MaxBackups, MaxAgeDays int; Compress bool }` with sensible defaults applied in `Load`.
- [x] 3.2 Update `config.example.yaml` with `logging.output`, `logging.format`, and `logging.file.*` (commented examples).
- [x] 3.3 Config tests: defaults populate when omitted; invalid `format` value is rejected by `validate`.

## 4. Runtime root plumbing

- [x] 4.1 In the server entrypoint, compute `dataDir = filepath.Join(d,"data")`, `logDir = filepath.Join(d,"logs")`, `skillsDir = filepath.Join(d,"skills")` from the `-d` value; remove `const DataDir = "data"`.
- [x] 4.2 Replace all `DataDir`/`"skills"` literal call sites: `fs.New(dataDir)`, `xizhi.RegisterAll(reg, dataDir, …)`, `NewSessionHandler(..., dataDir)`, `skill.NewLoader(skillsDir, …)`, and the executor global-skills argument.
- [x] 4.3 Create `{logDir}` (`os.MkdirAll(..., 0o755)`) **before** `logger.Init`, and `{dataDir}`/`{skillsDir}` as today via `fs.New` / loader.
- [x] 4.4 Reorder bootstrap to: parse flags → `config.Load(-f)` → `MkdirAll({logDir})` → `logger.Init(...)` → stores under `{dataDir}` → landlock → services → server.

## 5. Cobra CLI (`cmd/blowball`)

- [x] 5.1 Create `cmd/blowball/main.go`: cobra root command with persistent flags `-f`/`--config` (default `config.yaml`) and `-d`/`--data-dir` (default `.`); no `Run` on root so it prints help and exits non-zero when invoked without a subcommand.
- [x] 5.2 Implement `serve` subcommand: move the server bootstrap out of `cmd/server/main.go` into `serveRun` (taking the resolved `-f`/`-d`), preserving the graceful-shutdown signal handling.
- [x] 5.3 Implement `seed` subcommand: port `cmd/seed` (username/password/status/cost/dry-run flags + logic) into `seedRun`, reusing `-f` for the config path.
- [x] 5.4 Ensure unknown flags / bad usage produce cobra's usage error and a non-zero exit; `--help` works on root and both subcommands.
- [x] 5.5 Remove `cmd/server` and `cmd/seed` packages.

## 6. Landlock widening (`internal/tool/xizhi`)

- [x] 6.1 Update the `ApplyLandlock` call to target the runtime root per the 1.2 decision (per-subdir preferred, else `{d}`).
- [x] 6.2 Linux-only test: under landlock, write enough to trigger a lumberjack rotation and assert a new log file is created and written to (guards the D6 reopen-after-sandbox path).

## 7. Build, docs, gitignore

- [x] 7.1 `Makefile`: `make build` produces `bin/blowball`; `make run` runs `./bin/blowball serve`; add a `make seed` convenience target if useful.
- [x] 7.2 Add `/logs/` to `.gitignore` (runtime artifact, alongside `/data/` and `/skills/`).
- [x] 7.3 Update `CLAUDE.md`: commands (`blowball serve` / `blowball seed`), the `-d` directory layout, the new bootstrap order, and the landlock-scope note.
- [x] 7.4 Update `README.md`: usage with `-f`/`-d`, directory layout, and `logging.*` config.

## 8. Verification

- [x] 8.1 `make lint` and `make test` (with `-race`) pass.
- [x] 8.2 Manual: `./bin/blowball serve` with no flags reads `./config.yaml`, writes `./data` + `./logs`, reads `./skills` (backward-compatible defaults).
- [x] 8.3 Manual: `./bin/blowball serve -d /tmp/bb-root -f config.yaml` places data/logs/skills under `/tmp/bb-root`; force a log rotation and confirm rotated files appear.
- [x] 8.4 Manual: `./bin/blowball seed -username smoke -password pw -dry-run` prints the bcrypt hash without writing.
- [x] 8.5 Manual: SIGTERM to `serve` still drains and shuts down gracefully under cobra.
