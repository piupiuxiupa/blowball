## MODIFIED Requirements

### Requirement: Agent as tool via function calling
Confucius SHALL 通过 OpenAI function-calling 机制调度子 Agent，子 Agent 派发统一经由 `spawn_subagent` 工具（参数契约见 dynamic-subagents 能力）。

子 Agent 执行失败且被放弃（不重试、重试耗尽或取消）时，返回给 Confucius 的 tool result SHALL 携带「错误文本 + 已积累的部分输出」：子 Agent `Run` 在错误返回值中提供部分输出（失败轮已流出的 assistant 文本，或 continuation 耗尽时跨尝试累积的部分内容）且该内容非空白时，tool result content SHALL 为错误文本与部分输出的拼接（错误文本在前，以固定标记行分隔）；部分输出为空白时，tool result content SHALL 与错误文本字节级一致。失败结果 SHALL 保持 `isError` 语义且不套用 tool-result-envelope。

#### Scenario: Confucius receives function call
- **WHEN** OpenAI 返回包含 tool_calls 的响应，function name 为 spawn_subagent
- **THEN** 系统解析 parameters 中的 task、context、name、tools、preset、resume_agent_id，创建通用子 Agent 执行任务

#### Scenario: Tool call result returned to Confucius
- **WHEN** 子 Agent 执行完成（成功或失败）
- **THEN** 结果作为 tool role message 追加到 Confucius 的消息列表，Confucius 进行下一轮决策；失败时 content 为「错误文本 + 已积累部分输出」的合并文本

#### Scenario: Failed sub-agent carries partial output
- **WHEN** 子 Agent Run 中途 LLM 调用失败（如 llm_error、length_exhausted）且失败前已有 assistant 文本流出
- **THEN** 放弃后返回 Confucius 的 tool result content 由错误文本、固定分隔标记 `--- partial output before failure ---` 与部分输出组成，且 `isError` 为 true

#### Scenario: Blank partial output keeps legacy error text
- **WHEN** 子 Agent 失败且 Run 返回的部分输出为空白（如 round_cap_exhausted 无可抢救内容）
- **THEN** tool result content 与错误文本字节级一致（零行为回归）

#### Scenario: Retry give-up uses the last attempt's partial output
- **WHEN** 子 Agent 首次失败后经历重试，最终重试耗尽放弃
- **THEN** tool result 携带的是最后一次尝试的部分输出；中间失败尝试的 `agent_error`（Meta.retry=true）事件不携带部分输出

#### Scenario: Partial output visible across turn boundary
- **WHEN** 失败子 agent 调用的 tool_result 事件被持久化，后续 turn 重建历史
- **THEN** Confucius 上下文中的 spawn tool_call/tool_result 对携带同一合并文本；子 agent 名下的事件行不参与历史重建（既有约定不变）

### Requirement: Independent agent context
子 Agent SHALL 在独立上下文中运行。全新派发只接收 Confucius 传递的 task description 与 context；续跑派发（`resume_agent_id`）以该实例的持久化历史快照为起点追加新任务消息（见 dynamic-subagents 能力）。

#### Scenario: Sub-agent receives isolated context
- **WHEN** Confucius 全新派发一个子 Agent
- **THEN** 子 Agent 的消息列表仅包含：自身 system_prompt + 一条 user message（内容为 task + context），不包含用户的完整历史对话

#### Scenario: Resumed sub-agent starts from snapshot
- **WHEN** Confucius 续跑一个既有子 Agent 实例
- **THEN** 子 Agent 的消息列表为该实例持久化历史快照 + 新任务 user message，仍不包含用户完整历史对话

### Requirement: Agent configuration from file
主 Agent 与通用子 Agent 的 name、system_prompt、tools 列表、mcp 配置、skills 配置、output_schema 及 max_rounds（tool-calling 循环上限）SHALL 从 config.yaml 加载，其中 tools 列表中的名称可以解析为内置工具或已通过 MCP client 注册的外部 MCP 代理工具。子 Agent 配置 SHALL 位于 `agents.subagent` 段（通用默认值 + max_depth/max_concurrent/max_total_per_turn 预算 + 可选命名 presets），固定角色段 `agents.chongzhi` / `agents.liang` SHALL NOT 再被接受。agent 配置 SHALL NOT 包含任何 LLM 参数字段——模型、思考等级、输出配额（`max_tokens`/`max_completion_tokens`）与续写配置均为 turn 级属性，由模型目录 + 部署默认 + 请求参数解析得出（见 per-request-model-selection），并统一注入该 turn 的全部 agent。未设置或 `<= 0` 的 `max_rounds` SHALL 回退到默认值 `100`。

#### Scenario: Load agent config on startup
- **WHEN** 服务启动
- **THEN** 系统从 config.yaml 加载主 Agent 与 `agents.subagent` 通用配置（含预算与 presets），并从合并后的工具注册表（内置工具 + 外部 MCP 代理工具）解析 tools 列表

#### Scenario: Legacy role sections rejected
- **WHEN** config.yaml 仍包含 `agents.chongzhi` 或 `agents.liang` 段
- **THEN** 配置加载失败，错误信息指向 `agents.subagent` 迁移

#### Scenario: Configurable tool permissions
- **WHEN** 派发未收窄且子 Agent 有效工具集为空
- **THEN** 该子 Agent 调用 LLM 时不传递 tools 参数

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

## REMOVED Requirements

### Requirement: Flat agent topology
**Reason**: 固定扁平拓扑与固定角色调度被可配置深度的通用子 Agent 派发取代（见 dynamic-subagents 能力的 Tree-wide budgets 与 Single spawn_subagent dispatch tool）。
**Migration**: 调度入口迁移到 `spawn_subagent`；`max_depth` 默认 1 保持行为等价的扁平拓扑，需要嵌套时调高配置。
