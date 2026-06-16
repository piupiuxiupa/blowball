## ADDED Requirements

### Requirement: Agent MCP configuration structure
`config.yaml` 的每个 agent 段 SHALL 支持 `mcp` 字段，用于声明该 agent 可访问的 MCP server 及工具。

#### Scenario: Declare allowed MCP servers
- **WHEN** `agents.confuse.mcp.servers` 包含一个或多个 server 配置
- **THEN** 系统启动时解析并校验这些配置

#### Scenario: Declare allowed tools per server
- **WHEN** `agents.confuse.mcp.servers[].tools` 包含具体工具名
- **THEN** 系统仅允许该 agent 使用这些工具

#### Scenario: Use wildcard for all tools
- **WHEN** `agents.confuse.mcp.servers[].tools` 为 `["*"]`
- **THEN** 该 agent 可以使用对应 server 的全部工具

### Requirement: MCP server reference validation
Agent 的 `mcp.servers[].name` SHALL 引用全局 `mcp.servers` 中已声明的 server，否则启动失败。

#### Scenario: Reference existing server
- **WHEN** agent 配置引用的 server 名存在于全局 `mcp.servers`
- **THEN** 校验通过

#### Scenario: Reference missing server
- **WHEN** agent 配置引用的 server 名不存在于全局 `mcp.servers`
- **THEN** 系统启动失败并报告未知 server

### Requirement: MCP tool reference validation
Agent 的 `mcp.servers[].tools` 中每个具体工具名 SHALL 对应该 server 远端 `tools/list` 返回的工具，否则启动失败。

#### Scenario: Reference existing tool
- **WHEN** agent 配置引用的工具名存在于对应 server 的 `tools/list`
- **THEN** 校验通过

#### Scenario: Reference missing tool
- **WHEN** agent 配置引用的工具名不存在
- **THEN** 系统启动失败并报告未知工具

### Requirement: Per-agent MCP tool visibility
仅当 Agent 的 `mcp.servers` 明确允许某 server 时，该 server 的工具才对该 Agent 可见。

#### Scenario: Allowed server tools visible
- **WHEN** agent 配置允许某 server
- **THEN** 该 agent 的工具列表和系统提示词包含对应工具

#### Scenario: Unconfigured server tools hidden
- **WHEN** agent 配置未允许某 server
- **THEN** 该 server 的工具不出现在该 agent 的工具列表和系统提示词中
