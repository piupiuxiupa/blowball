# Executor Tools Capability

## Purpose

TBD — Provides sandboxed command and code execution tools (`bash`, `python`) for agents, scoped to the user's workspace with configurable isolation, audit logging, and dangerous-command detection.

## Requirements

### Requirement: Bash command execution tool
The system SHALL register a tool named `bash` that executes a shell command inside a bubblewrap sandbox scoped to the user's workspace and read-only skill directories.

#### Scenario: Successful bash command
- **WHEN** the agent calls `bash` with `{"command": "echo hello"}`
- **THEN** the system executes `bash -c 'echo hello'` inside a bwrap sandbox with working directory `/workspace` bound to the user's workspace
- **AND** the global skills directory is mounted read-only at `/skills/global`
- **AND** the per-user skills directory is mounted read-only at `/skills/user`
- **AND** the tool returns the command's stdout and stderr combined with the exit code

#### Scenario: Bash command timeout
- **WHEN** the agent calls `bash` with a command that runs longer than the configured timeout
- **THEN** the system terminates the sandbox process
- **AND** the tool returns an error indicating the command timed out

#### Scenario: Bash command output limit
- **WHEN** the agent calls `bash` and the combined output exceeds `max_output_bytes`
- **THEN** the system truncates the output to `max_output_bytes`
- **AND** the tool returns the truncated output with a marker indicating truncation

#### Scenario: Bash command outside workspace and skill directories access denied
- **WHEN** the sandboxed command attempts to read or write a path outside `/workspace`, `/skills/global`, or `/skills/user`
- **THEN** the access is denied by the bwrap filesystem isolation
- **AND** the tool returns the command's error output

### Requirement: Python code execution tool
The system SHALL register a tool named `python` that executes Python code inside a bubblewrap sandbox scoped to the user's workspace and read-only skill directories.

#### Scenario: Successful python code execution
- **WHEN** the agent calls `python` with `{"code": "print(1+1)"}`
- **THEN** the system executes `python3 -c 'print(1+1)'` inside a bwrap sandbox with working directory `/workspace`
- **AND** the global skills directory is mounted read-only at `/skills/global`
- **AND** the per-user skills directory is mounted read-only at `/skills/user`
- **AND** the tool returns the command's stdout and stderr combined with the exit code

#### Scenario: Python file execution
- **WHEN** the agent calls `python` with `{"file": "train.py"}`
- **THEN** the system executes `python3 /workspace/train.py` inside the sandbox
- **AND** the tool returns the command's stdout and stderr combined with the exit code

#### Scenario: Python file execution from skill directory
- **WHEN** the agent calls `python` with `{"file": "/skills/global/ifind-finance-data/call.py"}`
- **THEN** the system executes `python3 /skills/global/ifind-finance-data/call.py` inside the sandbox
- **AND** the file is readable because `/skills/global` is mounted read-only
- **AND** the tool returns the command's stdout and stderr combined with the exit code

#### Scenario: Python code network access denied by default
- **WHEN** the agent calls `python` with `{"code": "import socket; socket.create_connection(('example.com', 80))"}` and `network` is disabled
- **THEN** the connection attempt fails because the sandbox has no network access
- **AND** the tool returns the command's error output

### Requirement: Python package installation tool
The system SHALL register a tool named `pip_install` that installs Python packages into the user's workspace using pip inside the bubblewrap sandbox.

#### Scenario: Successful package installation
- **WHEN** the agent calls `pip_install` with `{"packages": ["requests"], "upgrade": false}`
- **THEN** the system executes `pip install --target /workspace/.pip requests` inside the bwrap sandbox
- **AND** the package is installed under `data/{userID}/workspace/.pip/`
- **AND** the tool returns the pip stdout/stderr combined with the exit code

#### Scenario: Install multiple packages
- **WHEN** the agent calls `pip_install` with `{"packages": ["numpy", "pandas>=2.0"], "upgrade": false}`
- **THEN** the system installs all listed packages into `/workspace/.pip`
- **AND** the tool returns the combined output

#### Scenario: Upgrade installed packages
- **WHEN** the agent calls `pip_install` with `{"packages": ["requests"], "upgrade": true}`
- **THEN** the system executes `pip install --target /workspace/.pip --upgrade requests`
- **AND** the package is upgraded in `/workspace/.pip`

#### Scenario: pip_install tool description guides model usage
- **WHEN** the system renders OpenAI tools for an agent configured with `pip_install`
- **THEN** the tool description instructs the agent to use it when Python code fails with `ModuleNotFoundError` or `ImportError`

#### Scenario: pip_install is not registered when disabled
- **WHEN** `tools.executor.pip.enabled` is `false`
- **THEN** the `pip_install` tool is not registered in the tool registry

#### Scenario: pip_install requires network
- **WHEN** the agent calls `pip_install` and `tools.executor.pip.network` is `true`
- **THEN** the bwrap command does not include `--unshare-net`
- **AND** pip can reach the configured index URL

#### Scenario: pip_install uses configured PyPI mirror
- **WHEN** `tools.executor.pip.index_url` is set to `https://pypi.tuna.tsinghua.edu.cn/simple`
- **THEN** the system passes `-i https://pypi.tuna.tsinghua.edu.cn/simple` to pip

#### Scenario: pip_install uses extra index URLs
- **WHEN** `tools.executor.pip.extra_index_urls` contains `https://extra.example.com/simple`
- **THEN** the system passes `--extra-index-url https://extra.example.com/simple` to pip

#### Scenario: pip_install uses trusted hosts
- **WHEN** `tools.executor.pip.trusted_hosts` contains `pypi.tuna.tsinghua.edu.cn`
- **THEN** the system passes `--trusted-host pypi.tuna.tsinghua.edu.cn` to pip

#### Scenario: pip_install audit logging
- **WHEN** the agent calls `pip_install`
- **THEN** the system logs the command string, tool name, user ID, exit code, output byte size, and duration

### Requirement: Installed packages visible to python tool
The system SHALL make packages installed via `pip_install` available to the `python` tool without requiring the agent to modify `sys.path`.

#### Scenario: Python code imports installed package
- **WHEN** the agent has previously called `pip_install` with `{"packages": ["requests"], "upgrade": false}`
- **AND** the agent calls `python` with `{"code": "import requests; print(requests.__version__)"}`
- **THEN** the code executes successfully
- **AND** the output contains the installed version of `requests`

#### Scenario: PYTHONPATH set in python sandbox
- **WHEN** the agent calls `python`
- **THEN** the bwrap sandbox has `PYTHONPATH=/workspace/.pip` (or the existing `PYTHONPATH` appended with `/workspace/.pip`)

### Requirement: Executor configuration
The system SHALL read executor configuration from `config.yaml` under `tools.executor.bash`, `tools.executor.python`, and `tools.executor.pip`.

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

#### Scenario: Enable pip_install tool
- **WHEN** `tools.executor.pip.enabled` is `true`
- **THEN** the `pip_install` tool is registered in the tool registry and visible to configured agents

#### Scenario: Configure pip timeout and output limit
- **WHEN** `tools.executor.pip.timeout` is set to `120s` and `max_output_bytes` is `65536`
- **THEN** pip commands are terminated after 120 seconds
- **AND** output is truncated at 65536 bytes

#### Scenario: Configure pip mirror
- **WHEN** `tools.executor.pip.index_url` is set to `https://pypi.tuna.tsinghua.edu.cn/simple`
- **THEN** `pip_install` uses that URL as the package index

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

### Requirement: Sandbox /tmp mapped to workspace tmp directory
The `bash` and `python` sandboxes SHALL mount the user's `workspace/tmp/` directory at `/tmp` inside the sandbox so that temporary files persist after the sandbox exits and are reachable via `xizhi_*` workspace tools.

#### Scenario: Bash writes temporary file to /tmp
- **WHEN** the agent calls `bash` with `{"command": "echo hello > /tmp/hello.txt"}`
- **THEN** the file is written to `data/{user_uuid}/workspace/tmp/hello.txt`
- **AND** a subsequent `xizhi_read_file` with path `tmp/hello.txt` returns the content

#### Scenario: Python writes temporary file to /tmp
- **WHEN** the agent calls `python` with `{"code": "open('/tmp/out.txt','w').write('x')"}`
- **THEN** the file is written to `data/{user_uuid}/workspace/tmp/out.txt`
- **AND** a subsequent `xizhi_read_file` with path `tmp/out.txt` returns the content

#### Scenario: workspace/tmp created on demand
- **WHEN** the agent calls `bash` and `workspace/tmp/` does not yet exist
- **THEN** the system creates `workspace/tmp/` before mounting the sandbox
- **AND** the command succeeds
