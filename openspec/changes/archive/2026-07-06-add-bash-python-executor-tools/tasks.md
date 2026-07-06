## 1. Configuration and models

- [x] 1.1 Add `ExecutorToolConfig` and `ExecutorConfig` structs to `internal/config/config.go` under `ToolsConfig`
- [x] 1.2 Define default values for timeout, max_output_bytes, allowed_env_patterns, network, enabled
- [x] 1.3 Update `config.example.yaml` with `tools.executor.bash` and `tools.executor.python` examples

## 2. Core executor package

- [x] 2.1 Create `internal/tool/executor/` package with `RegisterAll`, `Tools`, `Config` types
- [x] 2.2 Implement bwrap availability check (`bwrap --version` or equivalent) and platform gating
- [x] 2.3 Implement `buildBwrapArgs` to construct `--bind`, `--ro-bind`, `--unshare-*`, `--chdir`, `--die-with-parent` flags
- [x] 2.4 Implement environment variable filtering by `allowed_env_patterns`
- [x] 2.5 Implement command execution runner with `exec.CommandContext`, timeout, stdout/stderr capture
- [x] 2.6 Implement output truncation at `max_output_bytes` with truncation marker
- [x] 2.7 Implement audit logging with command, tool name, user ID, exit code, output size, duration
- [x] 2.8 Implement dangerous command pattern detection and warning log

## 3. Tool registration

- [x] 3.1 Implement `bash` tool spec with JSON schema for `command` parameter
- [x] 3.2 Implement `python` tool spec with JSON schema for `code` and `file` parameters
- [x] 3.3 Register tools in `cmd/server/main.go` when `tools.executor.*.enabled` is true and bwrap is available
- [x] 3.4 Ensure tools are scoped to requesting user's workspace via `fsStore.UserWorkspace(userID)`

## 4. Integration with existing systems

- [x] 4.1 Verify executor tools appear in `GET /api/v1/mcp/tools` response
- [x] 4.2 Verify `tool_call` / `tool_result` SSE events include bash/python execution results
- [x] 4.3 Ensure agent tool configuration validation accepts `bash` and `python` as valid tool names

## 5. Testing

- [x] 5.1 Add unit tests for environment variable filtering
- [x] 5.2 Add unit tests for dangerous command detection
- [x] 5.3 Add unit tests for output truncation
- [x] 5.4 Add unit tests for bwrap argument construction
- [x] 5.5 Add Linux-only integration test for actual bash/python execution inside bwrap
- [x] 5.6 Add test verifying workspace isolation (cannot read `/etc/shadow` or escape `/workspace`)
- [x] 5.7 Add test verifying network isolation when `network: false`

## 6. Documentation

- [x] 6.1 Update `CLAUDE.md` with executor tool description and security requirements
- [x] 6.2 Document bwrap installation prerequisite for Linux deployments
- [x] 6.3 Document `tools.executor` configuration options and examples

## 7. Verification

- [x] 7.1 Run `make lint` with no errors
- [x] 7.2 Run `make test` with all new tests passing
- [x] 7.3 Run integration tests on Linux environment
- [x] 7.4 Verify config loading with new `tools.executor` section
