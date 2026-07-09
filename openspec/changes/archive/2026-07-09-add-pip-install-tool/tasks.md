## 1. Configuration

- [x] 1.1 Add `PipToolConfig` struct to `internal/config/config.go` with `index_url`, `extra_index_urls`, and `trusted_hosts` fields.
- [x] 1.2 Extend `ExecutorConfig` to include a `Pip` field of type `PipToolConfig`.
- [x] 1.3 Add `DefaultPipToolConfig()` returning a default with `enabled: false`, `timeout: 120s`, `network: true`, and standard allowed env patterns.
- [x] 1.4 Call `cfg.Tools.Executor.Pip.ApplyDefaults()` in `config.Load()`.
- [x] 1.5 Add pip example block to `config.example.yaml` with mirror configuration comments.

## 2. Tool registration and schema

- [x] 2.1 Add `ToolPip = "pip_install"` constant in `internal/tool/executor/executor.go`.
- [x] 2.2 Add `registerPip` function in `internal/tool/executor/register.go` with JSON schema for `packages` (array of strings) and `upgrade` (bool).
- [x] 2.3 Implement pip argument parsing and command construction (`pip install --target /workspace/.pip ...`).
- [x] 2.4 Register `pip_install` in `RegisterAll` when `tools.cfg.Pip.Enabled` is true.

## 3. Sandbox environment

- [x] 3.1 Modify `buildBwrapArgs` in `internal/tool/executor/bwrap.go` to inject `PYTHONPATH=/workspace/.pip` (or append to existing `PYTHONPATH`) for executor tools.
- [x] 3.2 Ensure `/workspace/.pip` directory is created before pip runs (in the tool execution path or at workspace creation time).
- [x] 3.3 Verify `pip_install` respects `network: true/false` and the configured `index_url`, `extra_index_urls`, and `trusted_hosts`.

## 4. Startup wiring

- [x] 4.1 Update `cmd/server/main.go` to check bwrap availability when `Bash.Enabled || Python.Enabled || Pip.Enabled`.
- [x] 4.2 Ensure `executor.RegisterAll` registers `pip_install` when enabled.

## 5. Documentation and examples

- [x] 5.1 Update `CLAUDE.md` executor tools section to mention `pip_install`, its default network behavior, and mirror configuration.
- [x] 5.2 Update `api/openapi.yaml` if MCP tools list schema needs changes (likely not required).

## 6. Tests

- [x] 6.1 Add unit tests in `internal/tool/executor/` for pip command construction and argument parsing.
- [x] 6.2 Add unit tests for `PipToolConfig` defaults and bwrap `PYTHONPATH` injection.
- [x] 6.3 Add integration test that installs a small package (e.g. `colorama`) and verifies it is importable via the `python` tool on Linux.
- [x] 6.4 Ensure executor tests on macOS/Windows skip pip tests gracefully when bwrap is unavailable.
- [x] 6.5 Run `make test` and `make lint` and fix any failures.
