## MODIFIED Requirements

### Requirement: Agent configuration from file
每个 Agent 的 system_prompt、model、max_tokens、tools 列表 SHALL 从 config.yaml 加载，其中 tools 列表中的名称可以解析为内置工具或已通过 MCP client 注册的外部 MCP 代理工具。

#### Scenario: Load agent config on startup
- **WHEN** 服务启动
- **THEN** 系统从 config.yaml 的 agents 段加载所有 Agent 配置，并从合并后的工具注册表（内置工具 + 外部 MCP 代理工具）解析 tools 列表，构建 Agent 实例

#### Scenario: Configurable tool permissions
- **WHEN** Agent 配置中 tools 列表为空
- **THEN** 该 Agent 调用 OpenAI 时不传递 tools 参数

#### Scenario: Configured external MCP tool is available
- **WHEN** Agent 配置中 tools 列表包含一个外部 MCP 代理工具名，且该工具已成功注册
- **THEN** 系统构建 tool list 时包含该工具的定义，Agent 可正常调用

## ADDED Requirements

### Requirement: External MCP tool execution passthrough
Agent 通过 `tool.Registry.Call` 调用外部 MCP 代理工具时，系统 SHALL 将调用转发到对应 MCP server，并把结果以标准 tool role message 形式返回给 Agent。

#### Scenario: External tool call result returned to agent
- **WHEN** Agent 调用一个外部 MCP 代理工具
- **THEN** 系统通过 Registry 转发到 MCP client，完成远端调用后将结果追加到 Agent 消息列表
