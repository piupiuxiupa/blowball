## MODIFIED Requirements

### Requirement: Bash command execution tool
The system SHALL register a tool named `bash` that executes a shell command inside a bubblewrap sandbox scoped to the user's workspace and read-only skill directories.

#### Scenario: Successful bash command
- **WHEN** the agent calls `bash` with `{"command": "echo hello"}`
- **THEN** the system executes `bash -c 'echo hello'` inside a bwrap sandbox with working directory `/workspace` bound to the user's workspace
- **AND** the global skills directory is mounted read-only at `/skills/global`
- **AND** the per-user skills directory is mounted read-only at `/skills/user`
- **AND** the tool returns the command's stdout and stderr combined with the exit code

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

#### Scenario: Python file execution from skill directory
- **WHEN** the agent calls `python` with `{"file": "/skills/global/ifind-finance-data/call.py"}`
- **THEN** the system executes `python3 /skills/global/ifind-finance-data/call.py` inside the sandbox
- **AND** the file is readable because `/skills/global` is mounted read-only
- **AND** the tool returns the command's stdout and stderr combined with the exit code
