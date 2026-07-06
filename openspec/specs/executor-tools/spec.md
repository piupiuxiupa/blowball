# Executor Tools Capability

## Purpose

TBD — Provides sandboxed command and code execution tools (`bash`, `python`) for agents, scoped to the user's workspace with configurable isolation, audit logging, and dangerous-command detection.

## Requirements

### Requirement: Bash command execution tool
The system SHALL register a tool named `bash` that executes a shell command inside a bubblewrap sandbox scoped to the user's workspace.

#### Scenario: Successful bash command
- **WHEN** the agent calls `bash` with `{"command": "echo hello"}`
- **THEN** the system executes `bash -c 'echo hello'` inside a bwrap sandbox with working directory `/workspace` bound to the user's workspace
- **AND** the tool returns the command's stdout and stderr combined with the exit code

#### Scenario: Bash command timeout
- **WHEN** the agent calls `bash` with a command that runs longer than the configured timeout
- **THEN** the system terminates the sandbox process
- **AND** the tool returns an error indicating the command timed out

#### Scenario: Bash command output limit
- **WHEN** the agent calls `bash` and the combined output exceeds `max_output_bytes`
- **THEN** the system truncates the output to `max_output_bytes`
- **AND** the tool returns the truncated output with a marker indicating truncation

#### Scenario: Bash command outside workspace access denied
- **WHEN** the sandboxed command attempts to read or write a path outside `/workspace`
- **THEN** the access is denied by the bwrap filesystem isolation
- **AND** the tool returns the command's error output

### Requirement: Python code execution tool
The system SHALL register a tool named `python` that executes Python code inside a bubblewrap sandbox scoped to the user's workspace.

#### Scenario: Successful python code execution
- **WHEN** the agent calls `python` with `{"code": "print(1+1)"}`
- **THEN** the system executes `python3 -c 'print(1+1)'` inside a bwrap sandbox with working directory `/workspace`
- **AND** the tool returns the command's stdout and stderr combined with the exit code

#### Scenario: Python file execution
- **WHEN** the agent calls `python` with `{"file": "train.py"}`
- **THEN** the system executes `python3 /workspace/train.py` inside the sandbox
- **AND** the tool returns the command's stdout and stderr combined with the exit code

#### Scenario: Python code network access denied by default
- **WHEN** the agent calls `python` with `{"code": "import socket; socket.create_connection(('example.com', 80))"}` and `network` is disabled
- **THEN** the connection attempt fails because the sandbox has no network access
- **AND** the tool returns the command's error output

### Requirement: Executor configuration
The system SHALL read executor configuration from `config.yaml` under `tools.executor.bash` and `tools.executor.python`.

#### Scenario: Enable bash tool
- **WHEN** `tools.executor.bash.enabled` is `true`
- **THEN** the `bash` tool is registered in the tool registry and visible to configured agents

#### Scenario: Disable python tool
- **WHEN** `tools.executor.python.enabled` is `false`
- **THEN** the `python` tool is not registered

#### Scenario: Configure timeout and output limit
- **WHEN** `tools.executor.bash.timeout` is set to `30s` and `max_output_bytes` is `65536`
- **THEN** bash commands are terminated after 30 seconds
- **AND** output is truncated at 65536 bytes

### Requirement: Environment variable filtering
The system SHALL only pass environment variables matching `allowed_env_patterns` to sandboxed commands.

#### Scenario: Secret variable not leaked
- **WHEN** the host process has `OPENAI_API_KEY` set and the agent calls `bash` with `{"command": "env | grep OPENAI"}`
- **THEN** `OPENAI_API_KEY` is not present in the command output

#### Scenario: Allowed variable passed
- **WHEN** `allowed_env_patterns` includes `PATH` and `PATH` is set in the host environment
- **THEN** the sandboxed command sees `PATH`

### Requirement: Network isolation
The system SHALL run sandboxed commands without network access when `network` is disabled.

#### Scenario: Network disabled
- **WHEN** `tools.executor.bash.network` is `false`
- **THEN** the bwrap command includes `--unshare-net`

#### Scenario: Network enabled
- **WHEN** `tools.executor.bash.network` is `true`
- **THEN** the bwrap command does not include `--unshare-net`

### Requirement: Audit logging
The system SHALL emit a structured audit log entry for every command execution.

#### Scenario: Log bash execution
- **WHEN** the agent calls `bash`
- **THEN** the system logs the command string, tool name, user ID, exit code, output byte size, and duration

### Requirement: Dangerous command detection
The system SHALL detect dangerous command patterns and emit a warning log entry.

#### Scenario: Dangerous command warning
- **WHEN** the agent calls `bash` with `{"command": "rm -rf /workspace/build"}`
- **THEN** the command executes
- **AND** the system logs a warning that the command contains a dangerous pattern

### Requirement: Linux-only availability
The system SHALL only register executor tools on Linux systems where `bwrap` is installed and unprivileged user namespaces are available.

#### Scenario: Missing bwrap
- **WHEN** the server starts on Linux and `bwrap` is not in `PATH`
- **THEN** executor tools are not registered
- **AND** the system logs a fatal error indicating `bwrap` is required

#### Scenario: Non-Linux platform
- **WHEN** the server starts on macOS or Windows
- **THEN** executor tools are not registered
- **AND** startup continues without error
