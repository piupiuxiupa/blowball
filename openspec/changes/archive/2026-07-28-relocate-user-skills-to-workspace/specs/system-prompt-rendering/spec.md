## MODIFIED Requirements

### Requirement: Environment includes workspace, platform, OS, userID, cutoff, and skill directories
The `# Environment` section SHALL list the primary working directory, platform, OS, user ID, assistant knowledge cutoff, global skills directory, and per-user skills directory.

#### Scenario: System prompt includes all environment fields
- **WHEN** an agent is built for user `u-1` with workspace `/data/u-1/workspace`, global skills directory `/app/skills`, and user skills directory `/data/u-1/workspace/.blowball/skills`
- **THEN** its system prompt environment section includes the workspace path, `runtime.GOARCH`, `runtime.GOOS`, `u-1`, the knowledge cutoff, `/app/skills`, and `/data/u-1/workspace/.blowball/skills`

#### Scenario: Skill directories are shown as sandbox-resolvable paths
- **WHEN** the system prompt is rendered for an agent that will run inside a bubblewrap sandbox
- **THEN** the environment section exposes the global skills directory as `/skills/global` and the per-user skills directory as `/workspace/.blowball/skills`

### Requirement: Render with skills
The system prompt SHALL render a `## Skills` section with an XML-style `<skills>` catalog when one or more skills are configured. The section SHALL tell the agent it may use `bash` or `python` tools to read and execute files under the exposed skill directories. The section SHALL clarify that **global** skill directories are read-only and must not be modified; per-user skills live under the workspace at `.blowball/skills` and are managed exclusively via `luban_*` tools. `xizhi_*` tools MUST NOT be used to access `.blowball` or any skill directory.

#### Scenario: Skill catalog includes directory guidance
- **WHEN** an agent is configured with the `ifind-finance-data` skill
- **THEN** its system prompt skills section informs the agent that helper scripts are available under `/skills/global/ifind-finance-data/`
- **AND** it instructs the agent to use `bash` or `python` tools, not `xizhi_*` tools, to access those files
- **AND** it states that per-user skills reside at `/workspace/.blowball/skills` and are managed via `luban_*` tools
