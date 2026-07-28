## MODIFIED Requirements

### Requirement: Skill directories are accessible inside the sandbox
The system SHALL make the global skills directory available read-only inside the bubblewrap sandbox used by the `bash` and `python` tools at `/skills/global`. Per-user skills live under the user's workspace at `data/{userID}/workspace/.blowball/skills/`, which is already reachable inside the sandbox at `/workspace/.blowball/skills/` via the existing read-write `/workspace` bind; there is no separate `/skills/user` mount.

#### Scenario: Global skill script execution
- **WHEN** the agent calls `bash` with `{"command": "node /skills/global/ifind-finance-data/call-node.js ..."}`
- **THEN** the sandboxed process sees the global skill directory mounted read-only at `/skills/global`
- **AND** the command executes with read-only access to that path

#### Scenario: User skill script execution
- **WHEN** a user-installed skill exists under `data/{userID}/workspace/.blowball/skills/{skill-name}/`
- **AND** the agent calls `python` with `{"file": "/workspace/.blowball/skills/{skill-name}/helper.py"}`
- **THEN** the sandboxed process sees the per-user skill directory at `/workspace/.blowball/skills/`
- **AND** the command executes against that path

#### Scenario: Global skill directories are read-only
- **WHEN** the agent calls `bash` with `{"command": "touch /skills/global/ifind-finance-data/test.txt"}`
- **THEN** the command fails because `/skills/global` is mounted read-only

#### Scenario: No separate per-user skills mount exists
- **WHEN** the sandbox is constructed for a user with installed skills
- **THEN** there is no `/skills/user` mount point
- **AND** per-user skills are addressed exclusively under `/workspace/.blowball/skills/`
