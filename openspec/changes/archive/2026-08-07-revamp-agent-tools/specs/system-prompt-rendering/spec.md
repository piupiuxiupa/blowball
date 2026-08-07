## MODIFIED Requirements

### Requirement: Render with skills
The system prompt SHALL render a `## Skills` section with an XML-style `<skills>` catalog when one or more skills are configured. The section SHALL tell the agent it may use the `bash` tool to read and execute files under the exposed skill directories（`python`/`pip_install` 专用执行器已移除，技能目录文件的读取与执行统一经 `bash`，例如 `bash` 调用 `python3 <script>`）。The section SHALL clarify that **global** skill directories are read-only and must not be modified; per-user skills live under the workspace at `.blowball/skills` and are managed exclusively via `luban_*` tools. `xizhi_*` tools MUST NOT be used to access `.blowball` or any skill directory.

#### Scenario: Skill catalog includes directory guidance
- **WHEN** an agent is configured with the `ifind-finance-data` skill
- **THEN** its system prompt skills section informs the agent that helper scripts are available under `/skills/global/ifind-finance-data/`
- **AND** it instructs the agent to use the `bash` tool (not `xizhi_*` tools) to access those files
- **AND** it states that per-user skills reside at `/workspace/.blowball/skills` and are managed via `luban_*` tools
