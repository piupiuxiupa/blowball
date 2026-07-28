# Capability: System Prompt Rendering

## Purpose

TBD — System prompt rendering for the prompt package, providing clean separation from tool and skill registries.

## Requirements

### Requirement: Prompt package provides system prompt rendering
`internal/prompt` SHALL provide a function `RenderSystemPrompt` that renders a complete system prompt from plain input data.

#### Scenario: Render with environment only
- **WHEN** `RenderSystemPrompt` is called with `BasePrompt`, `Workspace`, `UserID`, `Platform`, `OS`, and `Cutoff`
- **THEN** the output contains a single `# Environment` section with all provided fields

#### Scenario: Render with tools
- **WHEN** `RenderSystemPrompt` is called with built-in tools and MCP tools grouped by server
- **THEN** the output contains a `## Built-in Tools` section and per-server `###` sections under `## MCP Tools`

#### Scenario: Render with skills
- **WHEN** `RenderSystemPrompt` is called with one or more skills
- **THEN** the output contains a `## Skills` section with an XML-style `<skills>` catalog

#### Scenario: Render omits empty sections
- **WHEN** `RenderSystemPrompt` is called with no tools and no skills
- **THEN** the output does not contain empty `## Built-in Tools`, `## MCP Tools`, or `## Skills` sections

### Requirement: Prompt package does not depend on tool or skill registries
`internal/prompt` SHALL NOT import `internal/tool`, `internal/tool/skill`, or any registry/loader type. It SHALL render only the data passed via `RenderInput`.

#### Scenario: Unit test without registry
- **WHEN** a test renders a system prompt using `RenderInput` with hand-crafted `ToolInfo` and `SkillInfo` values
- **THEN** the test succeeds without constructing a `tool.Registry` or `skill.Loader`

### Requirement: Environment is rendered exactly once
The final system prompt sent to the LLM SHALL contain exactly one `# Environment` section.

#### Scenario: Orchestrator builds agent system prompt
- **WHEN** `AgentFactory.Build` constructs an agent
- **THEN** the resulting `cfg.SystemPrompt` contains exactly one `# Environment` section

#### Scenario: OpenAI client converts messages
- **WHEN** `OpenAIClient` converts a system message to OpenAI parameters
- **THEN** it does not append an additional environment section

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

### Requirement: Workspace is passed explicitly, not read from context
`RenderSystemPrompt` SHALL receive `Workspace` as an input field. The rendering pipeline SHALL NOT read `ctx.Value("workspace")` to obtain the workspace path.

#### Scenario: Render without request context
- **WHEN** `RenderSystemPrompt` is called with a `Workspace` string and no context
- **THEN** it renders the workspace path correctly

### Requirement: Multi-form skill install guidance
When the system prompt renders a Skills section, it SHALL include guidance describing the install shapes supported by `luban_install_skill` and how to handle install-documentation URLs. Specifically, the guidance SHALL convey: (a) `luban_install_skill` can install a whole git repo, a single sub-skill selected via the `skill` parameter, or a single `SKILL.md`; (b) if a `.md` URL is not itself a skill, the tool returns the document content rather than installing, and the agent SHOULD read that content, determine the real source URL, and call `luban_install_skill` again with it; (c) when a user asks to install from an instruction page, the agent SHOULD follow that page to the real artifact instead of assuming the page itself is the skill.

#### Scenario: Install guidance describes the supported shapes
- **WHEN** the system prompt renders a Skills section for an agent with `luban_install_skill` available
- **THEN** the section describes whole-repo, sub-skill (`skill`), and single-SKILL.md installs

#### Scenario: Install guidance describes the install-doc flow
- **WHEN** the system prompt renders a Skills section
- **THEN** the section tells the agent that a non-skill `.md` URL is returned as an install document to read, and that the agent should follow it to the real source and re-install

#### Scenario: Agent follows an instruction page to the real source
- **WHEN** the user asks to install a skill from an instruction-page URL
- **THEN** the rendered guidance directs the agent to read the returned install-doc content and install the real source it points at, rather than treating the page as the skill
