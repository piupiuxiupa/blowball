## MODIFIED Requirements

### Requirement: Agent configuration from file
每个 Agent 的 system_prompt、model、max_tokens、tools 列表、mcp 配置及 skills 配置 SHALL 从 config.yaml 加载。

#### Scenario: Load agent config on startup
- **WHEN** 服务启动
- **THEN** 系统从 config.yaml 的 agents 段加载所有 Agent 配置，构建 Agent 实例

#### Scenario: Configurable tool permissions
- **WHEN** Agent 配置中 tools 列表为空且 mcp.servers 为空
- **THEN** 该 Agent 调用 OpenAI 时不传递 tools 参数

#### Scenario: Configurable MCP permissions
- **WHEN** Agent 配置中 mcp.servers 非空
- **THEN** 系统仅把允许的服务器及工具纳入该 Agent 的工具列表和系统提示词

#### Scenario: Configurable skill permissions
- **WHEN** Agent 配置中 skills 列表非空
- **THEN** 系统仅把这些 skill 纳入该 Agent 的系统提示词 skill catalog

## ADDED Requirements

### Requirement: AgentFactory requires userID
`AgentFactory.Build` SHALL 接收 `workspaceRoot` 和 `userID` 两个参数，以支持加载当前用户的 skill。

#### Scenario: Build agent for authenticated user
- **WHEN** Orchestrator 处理一个已认证用户的请求
- **THEN** 它使用用户的 userID 调用 `AgentFactory.Build`

#### Scenario: Build fails without userID when skills configured
- **WHEN** Agent 配置包含 skills 但 Build 时未提供 userID
- **THEN** 系统返回错误，提示缺少用户标识

### Requirement: Dynamic system prompt construction
每个 Agent 的完整系统提示词 SHALL 在 `AgentFactory.Build` 时动态构建，包含静态 system_prompt、环境信息、可用工具列表及可用 skill catalog。

#### Scenario: System prompt includes available tools
- **WHEN** Agent 构建成功
- **THEN** 其系统提示词包含内置工具及该 Agent 被允许的 MCP 工具的 name 与 description

#### Scenario: System prompt includes available skills
- **WHEN** Agent 配置允许使用 skill
- **THEN** 其系统提示词包含以 XML 格式组织的 skill catalog（name、description、location）及使用说明

#### Scenario: System prompt omits unavailable capabilities
- **WHEN** Agent 未配置任何 MCP server 或 skill
- **THEN** 系统提示词中不生成对应的空段落

### Requirement: Per-agent MCP tool filtering
`AgentFactory.Build` SHALL 根据 Agent 的 `mcp.servers` 配置，从全局 registry 中筛选出允许的 MCP 工具复制到 `reqReg`。

#### Scenario: Only allowed server tools are copied
- **WHEN** Agent 配置只允许 `remote_search` 服务器的 `web_search` 工具
- **THEN** `reqReg` 中仅包含该工具，不包含同一服务器的 `fetch_url` 或其他服务器的工具

#### Scenario: Wildcard allows all tools from a server
- **WHEN** Agent 配置中某 server 的 tools 为 `["*"]`
- **THEN** 该 server 的全部工具都被复制到 `reqReg`

#### Scenario: Unknown MCP server fails startup
- **WHEN** Agent 的 `mcp.servers[].name` 在全局 `mcp.servers` 中不存在
- **THEN** 系统启动校验失败并报告错误

#### Scenario: Unknown MCP tool fails startup
- **WHEN** Agent 的 `mcp.servers[].tools` 包含某不存在工具名
- **THEN** 系统启动校验失败并报告错误

### Requirement: Per-agent skill filtering
`AgentFactory.Build` SHALL 根据 Agent 的 `skills` 配置，从全局 skill 目录和当前用户 skill 目录中筛选出允许的 skill。

#### Scenario: Allowed global skill appears in catalog
- **WHEN** Agent 配置允许 `coding-style` 且全局 skill 目录存在 `coding-style/SKILL.md`
- **THEN** 该系统提示词 skill catalog 包含该 skill

#### Scenario: Allowed user skill appears in catalog
- **WHEN** Agent 配置允许 `qa-checklist` 且当前用户 skill 目录存在 `qa-checklist/SKILL.md`
- **THEN** 系统提示词 skill catalog 包含该 skill

#### Scenario: Unknown skill fails startup
- **WHEN** Agent 的 `skills` 列表包含一个不存在于全局或用户目录的 skill
- **THEN** 系统启动校验失败并报告错误

#### Scenario: User skill overrides global skill
- **WHEN** 全局目录和当前用户目录同时存在同名 skill
- **THEN** 系统使用当前用户的 skill，并在 catalog 中只出现一次
