## ADDED Requirements

### Requirement: Reserved workspace-internal directories are rejected
`xizhi_*` path validation SHALL reject any path whose first cleaned segment is a reserved application namespace directory (`.blowball`), so that workspace-resident application state — including per-user skills at `.blowball/skills/` — is reachable only through its dedicated tools (`luban_*`) and never through the file tools. The rejection SHALL use the same outside-workspace error style with relative-path guidance.

#### Scenario: Read under reserved directory blocked
- **WHEN** the agent calls `xizhi_read_file` with path `.blowball/skills/foo/SKILL.md`
- **THEN** the system rejects the operation with a path error
- **AND** the error guides the model to use `luban_*` tools for skills

#### Scenario: Write under reserved directory blocked
- **WHEN** the agent calls `xizhi_write_file` with path `.blowball/skills/foo/SKILL.md`
- **THEN** the system rejects the operation with a path error

#### Scenario: Non-reserved dotfiles remain allowed
- **WHEN** the agent calls `xizhi_read_file` with path `.env`
- **THEN** the system reads the file normally, because `.env` is not a reserved namespace directory
