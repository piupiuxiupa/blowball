## ADDED Requirements

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

### Requirement: Environment includes workspace, platform, OS, userID, and cutoff
The `# Environment` section SHALL list the primary working directory, platform, OS, user ID, and assistant knowledge cutoff.

#### Scenario: System prompt includes all environment fields
- **WHEN** an agent is built for user `u-1` with workspace `/data/u-1/workspace`
- **THEN** its system prompt environment section includes the workspace path, `runtime.GOARCH`, `runtime.GOOS`, `u-1`, and the knowledge cutoff

### Requirement: Workspace is passed explicitly, not read from context
`RenderSystemPrompt` SHALL receive `Workspace` as an input field. The rendering pipeline SHALL NOT read `ctx.Value("workspace")` to obtain the workspace path.

#### Scenario: Render without request context
- **WHEN** `RenderSystemPrompt` is called with a `Workspace` string and no context
- **THEN** it renders the workspace path correctly
