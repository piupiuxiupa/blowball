# Delta: agent-orchestration

## MODIFIED Requirements

### Requirement: Agent configuration from file
每个 Agent 的 name、system_prompt、tools 列表、mcp 配置、skills 配置、output_schema 及 max_rounds（tool-calling 循环上限）SHALL 从 config.yaml 加载，其中 tools 列表中的名称可以解析为内置工具或已通过 MCP client 注册的外部 MCP 代理工具。agent 配置 SHALL NOT 包含任何 LLM 参数字段——模型、思考等级、输出配额（`max_tokens`/`max_completion_tokens`）与续写配置均为 turn 级属性，由模型目录 + 部署默认 + 请求参数解析得出（见 per-request-model-selection），并统一注入该 turn 的全部 agent。未设置或 `<= 0` 的 `max_rounds` SHALL 回退到默认值 `100`。

#### Scenario: Load agent config on startup
- **WHEN** 服务启动
- **THEN** 系统从 config.yaml 的 agents 段加载所有 Agent 配置，并从合并后的工具注册表（内置工具 + 外部 MCP 代理工具）解析 tools 列表，构建 Agent 实例

#### Scenario: Configurable tool permissions
- **WHEN** Agent 配置中 tools 列表为空且 mcp.servers 为空
- **THEN** 该 Agent 调用 LLM 时不传递 tools 参数

#### Scenario: Configurable MCP permissions
- **WHEN** Agent 配置中 mcp.servers 非空
- **THEN** 系统仅把允许的服务器及工具纳入该 Agent 的工具列表和系统提示词

#### Scenario: Configurable skill permissions
- **WHEN** Agent 配置中 skills 列表非空
- **THEN** 系统仅把这些 skill 纳入该 Agent 的系统提示词 skill catalog

#### Scenario: Default max_rounds when unset
- **WHEN** 某 Agent 配置未设置 `max_rounds`（或设为 `<= 0`）
- **THEN** 该 Agent 的 tool-calling 循环上限 SHALL 取默认值 `100`

#### Scenario: agent 残留 max_tokens 拒绝加载
- **WHEN** config.yaml 的某 agent 段包含 `max_tokens`
- **THEN** 配置加载失败,错误信息指向 `openai.models[].max_completion_tokens` 迁移(残留拒绝见 per-request-model-selection)
