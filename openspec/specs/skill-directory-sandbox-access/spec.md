# Capability: Skill Directory Sandbox Access

## Purpose

TBD — Makes both the global skills directory and the per-user skills directory available read-only inside the bubblewrap sandbox used by the `bash` executor tool.

## Requirements

### Requirement: Skill directories are accessible inside the sandbox
The system SHALL make the global skills directory available read-only inside the bubblewrap sandbox used by the `bash` tool at `/skills/global`. Per-user skills live under the user's workspace at `data/{userID}/workspace/.blowball/skills/`, which is already reachable inside the sandbox at `/workspace/.blowball/skills/` via the existing read-write `/workspace` bind; there is no separate `/skills/user` mount.（`python`/`pip_install` 专用执行器已移除，技能目录的只读访问仅由 `bash` 承载；运行技能内 Python 脚本统一经 `bash` 调用 `python3`。）

#### Scenario: Global skill script execution
- **WHEN** the agent calls `bash` with `{"command": "node /skills/global/ifind-finance-data/call-node.js ..."}`
- **THEN** the sandboxed process sees the global skill directory mounted read-only at `/skills/global`
- **AND** the command executes with read-only access to that path

#### Scenario: User skill script execution
- **WHEN** a user-installed skill exists under `data/{userID}/workspace/.blowball/skills/{skill-name}/`
- **AND** the agent calls `bash` with `{"command": "python3 /workspace/.blowball/skills/{skill-name}/helper.py"}`
- **THEN** the sandboxed process sees the per-user skill directory at `/workspace/.blowball/skills/`

#### Scenario: Global skill directories are read-only
- **WHEN** the agent calls `bash` with `{"command": "touch /skills/global/ifind-finance-data/test.txt"}`
- **THEN** the command fails because `/skills/global` is mounted read-only

#### Scenario: No separate per-user skills mount exists
- **WHEN** the sandbox is constructed for a user with installed skills
- **THEN** there is no `/skills/user` mount point
- **AND** per-user skills are addressed exclusively under `/workspace/.blowball/skills/`
