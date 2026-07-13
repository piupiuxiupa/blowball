## ADDED Requirements

### Requirement: Home directory in sandbox
The `bash`, `python`, and `pip_install` sandboxes SHALL provide a real, writable `$HOME` directory inside the bubblewrap namespace so that commands which cache or configure under `$HOME` (e.g. pip `~/.cache`, `~/.config`) function correctly. The sandbox SHALL force the `HOME` environment variable to this synthetic home path regardless of `allowed_env_patterns`, overriding any host `HOME` that would otherwise leak into the sandbox.

#### Scenario: Home directory is writable
- **WHEN** the agent calls `bash` with `{"command": "echo x > $HOME/.cache/foo && cat $HOME/.cache/foo"}`
- **THEN** the command succeeds and prints `x`
- **AND** `$HOME` resolves to a mounted, writable path inside the sandbox

#### Scenario: HOME is forced to the synthetic path
- **WHEN** the agent calls `bash` with `{"command": "echo $HOME"}`
- **THEN** the output is the synthetic home path (e.g. `/home/blowball`), not the host user's home directory
- **AND** this holds even when `allowed_env_patterns` includes `HOME`

#### Scenario: Host HOME does not leak when filtered out
- **WHEN** `allowed_env_patterns` does not include `HOME`
- **AND** the agent calls `bash` with `{"command": "echo $HOME"}`
- **THEN** the output is still the synthetic home path (HOME is forced, not inherited)

### Requirement: Operator tools directory on PATH
The `bash`, `python`, and `pip_install` sandboxes SHALL mount the operator tools directory `{data-dir}/tools` read-only at `$HOME/.local/bin` (inside the synthetic home established above) and SHALL prepend `$HOME/.local/bin` to `PATH` so the operator's tools are invocable by bare name and take precedence over host `/usr/bin` entries. The `--tmpfs` establishing `$HOME` SHALL appear before the `--ro-bind` of the tools directory so the mountpoint exists.

#### Scenario: Operator tool invoked by bare name
- **WHEN** an executable `mytool` exists in `{data-dir}/tools`
- **AND** the agent calls `bash` with `{"command": "mytool --version"}`
- **THEN** the command resolves `mytool` from `$HOME/.local/bin` via `PATH` and executes it
- **AND** the tool's stdout/stderr combined with its exit code is returned

#### Scenario: Tools resolve via hardcoded $HOME/.local/bin lookup
- **WHEN** a tool in the sandbox looks up `$HOME/.local/bin/<binary>` directly (not via `PATH`)
- **AND** that binary exists in `{data-dir}/tools`
- **THEN** the lookup succeeds because `$HOME/.local/bin` is populated by the read-only bind mount

#### Scenario: Tools directory is read-only in the sandbox
- **WHEN** the agent calls `bash` with `{"command": "touch $HOME/.local/bin/evil"}`
- **THEN** the command fails because `$HOME/.local/bin` is mounted read-only

#### Scenario: PATH is prepended with tools bin
- **WHEN** the agent calls `bash` with `{"command": "echo $PATH"}`
- **THEN** the first `PATH` entry is `$HOME/.local/bin`
- **AND** the remainder is the inherited host `PATH` filtered by `allowed_env_patterns` (when `PATH` is allowed)

#### Scenario: Empty tools directory still sets up home and PATH
- **WHEN** `{data-dir}/tools` exists but is empty
- **AND** the agent calls `bash` with `{"command": "echo $PATH"}`
- **THEN** `$HOME/.local/bin` is still present and first on `PATH`
- **AND** `$HOME` is still the synthetic writable home
