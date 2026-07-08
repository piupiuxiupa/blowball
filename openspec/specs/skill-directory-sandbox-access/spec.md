# Capability: Skill Directory Sandbox Access

## Purpose

TBD — Makes both the global skills directory and the per-user skills directory available read-only inside the bubblewrap sandbox used by the `bash` and `python` executor tools.

## Requirements

### Requirement: Skill directories are accessible inside the sandbox
The system SHALL make both the global skills directory and the per-user skills directory available read-only inside the bubblewrap sandbox used by the `bash` and `python` tools.

#### Scenario: Global skill script execution
- **WHEN** the agent calls `bash` with `{"command": "node /skills/global/ifind-finance-data/call-node.js ..."}`
- **THEN** the sandboxed process sees the global skill directory mounted at `/skills/global`
- **AND** the command executes with read-only access to that path

#### Scenario: User skill script execution
- **WHEN** a user-installed skill exists under `data/{userID}/skills/{skill-name}/`
- **AND** the agent calls `python` with `{"file": "/skills/user/{skill-name}/helper.py"}`
- **THEN** the sandboxed process sees the per-user skill directory mounted at `/skills/user`
- **AND** the command executes with read-only access to that path

#### Scenario: Skill directories are read-only
- **WHEN** the agent calls `bash` with `{"command": "touch /skills/global/ifind-finance-data/test.txt"}`
- **THEN** the command fails because `/skills/global` is mounted read-only
