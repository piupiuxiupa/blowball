## Why

Agents frequently write Python code that depends on third-party packages (`numpy`, `pandas`, `requests`, etc.). Today the `python` executor tool runs in a network-off sandbox with a read-only `/usr`, so `ModuleNotFoundError` is unrecoverable. Adding a `pip_install` tool lets agents install missing packages on demand, with the installed packages persisted in the user's workspace and automatically available to subsequent `python` invocations.

## What Changes

- Add a new `pip_install` tool to the executor tool family.
- The tool runs `pip install --target /workspace/.pip <packages>` inside the existing bubblewrap sandbox.
- Installed packages persist under `data/{userID}/workspace/.pip` and are automatically exposed to the `python` tool via `PYTHONPATH`.
- Extend `config.yaml` under `tools.executor` with a new `pip` block supporting:
  - `enabled`, `timeout`, `max_output_bytes`, `allowed_env_patterns`, `network`
  - `index_url` for the PyPI mirror
  - `extra_index_urls` for additional indexes
  - `trusted_hosts` for HTTP mirrors / self-signed HTTPS
- Update `cmd/server/main.go` to register `pip_install` when enabled and validate bwrap availability.
- Update `config.example.yaml` with commented examples.
- Add unit and integration tests for successful installs, mirror configuration, and `PYTHONPATH` visibility.

## Capabilities

### New Capabilities

- None. This change extends an existing capability.

### Modified Capabilities

- `executor-tools`: Add a `pip_install` tool and corresponding `tools.executor.pip` configuration so agents can install Python packages on demand. The `python` tool remains unchanged from the agent's perspective, but its sandbox environment now includes packages installed via `pip_install`.

## Impact

- **Backend**: `internal/config/config.go`, `internal/tool/executor/*`, `cmd/server/main.go`, `config.example.yaml`.
- **Behavior**: Agents configured with `pip_install` can recover from `ModuleNotFoundError` autonomously.
- **Security**: `pip_install` executes arbitrary package build scripts inside the same bwrap sandbox as `bash`/`python`. It requires network access (`network: true` by default).
- **Platform**: Linux-only, same as existing executor tools; macOS/Windows development builds skip registration.
- **API**: No new HTTP endpoints; the tool is exposed through the existing tool registry and MCP tools list.
