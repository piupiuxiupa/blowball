# agent-orchestration Specification

## Purpose

定义多 Agent 系统的编排能力，包括 flat 拓扑、基于 function-calling 的调度、并行执行、上下文隔离、流式透传、配置加载、Confucius 主控循环以及 token 用量可观测性。

## Requirements

### Requirement: Flat agent topology
系统 SHALL 采用 flat 拓扑，仅 Confucius 可调度其他 Agent，子 Agent 不允许嵌套调用。

#### Scenario: Confucius dispatches sub-agents
- **WHEN** Confucius 通过 function-calling 调用 invoke_chongzhi 或 invoke_liang
- **THEN** 系统启动对应子 Agent 执行任务

#### Scenario: Sub-agents cannot call other agents
- **WHEN** Chongzhi 或 Liang 的 tool list 被构建
- **THEN** tool list 中不包含 invoke_chongzhi、invoke_liang 等其他 Agent 调度工具

### Requirement: Agent as tool via function calling
Confucius SHALL 通过 OpenAI function-calling 机制调度子 Agent，每个子 Agent 定义为一个 tool。

#### Scenario: Confucius receives function call
- **WHEN** OpenAI 返回包含 tool_calls 的响应，function name 为 invoke_chongzhi 或 invoke_liang
- **THEN** 系统解析 parameters 中的 task 和 context，启动对应子 Agent

#### Scenario: Tool call result returned to Confucius
- **WHEN** 子 Agent 执行完成（成功或失败）
- **THEN** 结果作为 tool role message 追加到 Confucius 的消息列表，Confucius 进行下一轮决策

### Requirement: Parallel agent execution
系统 SHALL 支持 LLM 自主决定的并行 Agent 调用。当 Confucius 的 LLM 响应包含多个 tool_calls 时，并行执行。

#### Scenario: Multiple tool calls executed in parallel
- **WHEN** OpenAI 返回包含 2 个以上 tool_calls 的响应
- **THEN** 系统使用 errgroup 并行启动所有子 Agent goroutine

#### Scenario: Parallel results collected
- **WHEN** 所有并行子 Agent 执行完成
- **THEN** 系统按 tool_call_id 对应关系收集所有结果，构造 tool response messages

#### Scenario: One agent fails in parallel execution
- **WHEN** 并行执行中一个子 Agent 失败
- **THEN** 失败信息作为错误 StreamEvent 流式通知，其他 Agent 继续执行，失败结果返回 Confucius 决策

### Requirement: Independent agent context
子 Agent SHALL 在独立上下文中运行，只接收 Confucius 传递的 task description 和 context。

#### Scenario: Sub-agent receives isolated context
- **WHEN** Confucius 调用子 Agent
- **THEN** 子 Agent 的消息列表仅包含：自身 system_prompt + 一条 user message（内容为 task + context），不包含用户的完整历史对话

### Requirement: Streaming passthrough
子 Agent 的响应 SHALL 通过共享 StreamEvent channel 透传到 SSE 输出。

#### Scenario: Sub-agent tokens streamed directly
- **WHEN** 子 Agent 调用 OpenAI streaming API 产生 token
- **THEN** token 作为 StreamEvent{Type: "token", Agent: "Chongzhi"} 写入共享 channel，SSE handler 直接推送给用户

#### Scenario: Agent lifecycle events
- **WHEN** 子 Agent 开始或结束执行
- **THEN** 系统推送 StreamEvent{Type: "agent_start"/"agent_end", Agent: "xxx"}

#### Scenario: Agent error streamed
- **WHEN** 子 Agent 执行过程中发生错误
- **THEN** 系统推送 StreamEvent{Type: "agent_error", Agent: "xxx", Content: "错误描述", Meta: {error_code: "..."}}，然后推送 agent_end 事件

### Requirement: Agent configuration from file
每个 Agent 的 system_prompt、model、max_tokens、tools 列表、mcp 配置、skills 配置、thinking 开关及 reasoning_effort 配置 SHALL 从 config.yaml 加载，其中 tools 列表中的名称可以解析为内置工具或已通过 MCP client 注册的外部 MCP 代理工具。

#### Scenario: Load agent config on startup
- **WHEN** 服务启动
- **THEN** 系统从 config.yaml 的 agents 段加载所有 Agent 配置，并从合并后的工具注册表（内置工具 + 外部 MCP 代理工具）解析 tools 列表，构建 Agent 实例

#### Scenario: Configurable tool permissions
- **WHEN** Agent 配置中 tools 列表为空且 mcp.servers 为空
- **THEN** 该 Agent 调用 OpenAI 时不传递 tools 参数

#### Scenario: Configurable MCP permissions
- **WHEN** Agent 配置中 mcp.servers 非空
- **THEN** 系统仅把允许的服务器及工具纳入该 Agent 的工具列表和系统提示词

#### Scenario: Configurable skill permissions
- **WHEN** Agent 配置中 skills 列表非空
- **THEN** 系统仅把这些 skill 纳入该 Agent 的系统提示词 skill catalog

### Requirement: Confucius agent loop
Confucius SHALL 实现多轮 tool-calling 循环，直到 LLM 返回 finish_reason 为 stop。

#### Scenario: Confucius calls tools then summarizes
- **WHEN** Confucius 首轮调用返回 tool_calls，执行后第二轮 LLM 返回 content 且 finish_reason 为 stop
- **THEN** Confucius 输出最终汇总内容，推送 done 事件

#### Scenario: Confucius handles directly
- **WHEN** Confucius 首轮调用直接返回 content 且 finish_reason 为 stop（无 tool_calls）
- **THEN** Confucius 直接输出内容，推送 done 事件

### Requirement: Token usage observability
系统 SHALL 在每次请求的 done 事件中发射 per-agent token 用量拆分，并按 `turn-cost-tracking` 能力持久化。done 事件 `Meta.usage` SHALL 采用嵌套形状 `{total, by_agent, meta}`，其中 `by_agent` 按每个参与 agent（Confucius 及其调度的子 agent）拆分 prompt_tokens/completion_tokens/total_tokens/reasoning_tokens。

#### Scenario: Done event carries per-agent breakdown
- **WHEN** 一次完整的用户请求处理完成（成功或失败路径）
- **THEN** StreamEvent{Type: "done"} 的 `Meta.usage` 包含 `total`（聚合）与 `by_agent`（per-agent 明细，键为 agent 名）两段
- **AND THEN** `by_agent` 至少包含 `Confucius`；若调度了子 agent，则同时包含对应子 agent 键

#### Scenario: Parallel sub-agent usage attributed separately
- **WHEN** 一个 turn 中 Confucius 并行调度了 Chongzhi 与 Liang
- **THEN** `usage.by_agent` 分别记录 Confucius、Chongzhi、Liang 三者的 token 消耗
- **AND THEN** 三者之和等于 `usage.total`

#### Scenario: Usage persisted per turn
- **WHEN** done 事件发射后，turn 的消息批次被持久化
- **THEN** 系统按 `turn-cost-tracking` 能力将同一 usage 对象写入 `turn_usage` 表（见 turn-cost-tracking spec）

#### Scenario: Error turn still reports usage
- **WHEN** orchestrator 处理失败（非客户端取消）
- **THEN** done 事件仍发射已累积的 usage（含 `total` 与已执行 agent 的 `by_agent`），`Meta.usage` 可选包含 `error` 字段描述失败原因

### Requirement: Per-agent usage attribution in sub-agent dispatch
Confucius 的 dispatch 循环 SHALL 在折叠子 agent usage 进 turn 总量的同时，保留 per-agent 拆分并沿调用链传递至 done 事件，修复当前实现将子 agent usage 压扁进父总量、导致 per-agent 拆分丢失的缺陷。

#### Scenario: Sub-agent usage preserved separately
- **WHEN** Confucius 通过 `dispatchSubAgent` 执行一个子 agent 并获得其 `subUsage`
- **THEN** 该子 agent 的 usage 同时被加入 per-agent 拆分 map（键为子 agent 名）与 turn 总量，而非仅折入总量

#### Scenario: Confucius own usage attributed to Confucius
- **WHEN** Confucius 自身的 LLM 调用产生 usage
- **THEN** 该 usage 归入 per-agent 拆分的 `Confucius` 键，与子 agent usage 分离

### Requirement: Structured sub-agent return contract
配置了 `output_schema` 的子 agent SHALL 在其 tool-calling 循环的终轮（模型不再 emit tool_call、即将返回最终内容给父级时）启用 OpenAI structured output（`response_format: json_schema`），使返回给父级的内容符合声明 schema。未配置 `output_schema` 的子 agent SHALL 返回自由文本。

#### Scenario: Sub-agent with output schema uses structured output on final round
- **WHEN** 一个配置了 `output_schema` 的子 agent（如 Liang）在其循环中进入终轮（`finish_reason=stop`，无 tool_call）
- **THEN** 该终轮的 LLM 请求携带 `response_format: json_schema`（内容为配置的 schema）
- **AND THEN** 子 agent 返回给父级的内容（toolResult.content）是该 schema 的合规 JSON

#### Scenario: Intermediate tool rounds not forced to structured output
- **WHEN** 配置了 `output_schema` 的子 agent 在中间轮仍需调用工具（emit tool_call）
- **THEN** 这些中间轮的 LLM 请求不携带 `response_format`，避免与 tool_call 冲突

#### Scenario: Reasoning model degrades to prompt-only constraint
- **WHEN** 配置了 `output_schema` 的子 agent 同时启用 `thinking: true`（reasoning 模型）
- **THEN** 系统跳过 API 强制的 `response_format`，改为在系统 prompt 中以文本约束要求返回该 schema 的 JSON（model-gate 降级）
- **AND THEN** 启动校验拒绝 `thinking:true` 且要求强制 structured output 的矛盾配置

#### Scenario: Sub-agent without output schema returns free text
- **WHEN** 一个未配置 `output_schema` 的子 agent（如 Chongzhi）完成执行
- **THEN** 其返回内容为自由文本，行为与变更前一致

#### Scenario: Parent informed of structured return shape
- **WHEN** Confucius 的工具列表中包含一个会返回结构化 JSON 的子 agent invoke 工具
- **THEN** Confucius 的系统 prompt 声明该子 agent 返回结构化 JSON，辅助其综合质量

### Requirement: Transient error retry for sub-agent dispatch
系统 SHALL 对子 agent dispatch 的瞬时错误（LLM 429/5xx/超时、瞬时 tool_error）执行受限重试，并对语义错误（`bad_args`、`unknown_tool`）永不重试。重试策略 SHALL 按 agent 幂等性区分：只读子 agent（Liang）默认可重试；有副作用的子 agent（Chongzhi）仅当本次执行尚未触发任何成功 tool_call 时可重试。

#### Scenario: Transient LLM error retried for read-only agent
- **WHEN** Liang 的 LLM 调用返回 429 或超时
- **THEN** 系统按指数退避重试（至多配置的 max_attempts，默认 2），复用相同 invoke 参数
- **AND THEN** 重试时发射 `agent_error` 事件且 `Meta.retry=true` 以通知前端

#### Scenario: Semantic error never retried
- **WHEN** 子 agent dispatch 因 `bad_args` 或 `unknown_tool` 失败
- **THEN** 系统立即返回错误结果，不重试（相同参数必再败）

#### Scenario: Side-effecting agent not retried after tool execution
- **WHEN** Chongzhi 已成功执行至少一个 xizhi 工具调用（如 write_file）后，其后续 LLM 调用失败
- **THEN** 系统不重试（已产生文件系统副作用，重试将重复执行）
- **AND THEN** 错误结果喂回 Confucius 决策

#### Scenario: Side-effecting agent retried only before any tool call
- **WHEN** Chongzhi 首轮 LLM 调用即失败（尚未触发任何 tool_call）
- **THEN** 系统可重试该 LLM 调用（无副作用风险），重试上限受重试预算约束

#### Scenario: Retry budget enforced
- **WHEN** 一个 turn 内累计重试消耗的 token 达到配置预算上限
- **THEN** 系统停止后续重试，将当前错误结果喂回 Confucius（依赖 `turn-cost-tracking` 数据防失控）

#### Scenario: Retry exhausted surfaces error to parent
- **WHEN** 重试次数耗尽仍失败
- **THEN** 系统将最终错误作为 tool result 喂回 Confucius，由其决定后续（重派/降级/告知用户）

### Requirement: Parallel dispatch decision guidance in system prompt
Confucius 的系统 prompt SHALL 包含并行调度决策指导，使模型在独立子任务时于单个 assistant turn 内 emit 多个 tool_calls，在有依赖时串行，并遵守并行预算。

#### Scenario: Guidance present in Confucius prompt
- **WHEN** Confucius 的系统 prompt 被构建
- **THEN** prompt 文本包含并行决策指导：独立子任务单 turn 并行；有依赖才串行；并行预算 2-3、避免 >5；禁止对同一子 agent 发重叠任务

#### Scenario: Sequential dispatch when dependency exists
- **WHEN** 任务 B 的输入依赖任务 A 的输出
- **THEN** 指导文本明确要求模型先完成 A、拿到结果后再发起 B（不在同一 turn 并行 emit）

### Requirement: External MCP tool execution passthrough
Agent 通过 `tool.Registry.Call` 调用外部 MCP 代理工具时，系统 SHALL 将调用转发到对应 MCP server，并把结果以标准 tool role message 形式返回给 Agent。

#### Scenario: External tool call result returned to agent
- **WHEN** Agent 调用一个外部 MCP 代理工具
- **THEN** 系统通过 Registry 转发到 MCP client，完成远端调用后将结果追加到 Agent 消息列表

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

### Requirement: Orchestrator receives full conversation history at the start of each turn
The orchestrator SHALL accept the complete session conversation history recovered from persistence, combined with the current user message, and pass it to the Confucius agent loop as the initial `messages` slice.

#### Scenario: OrchestratorRunner.Handle signature carries history
- **WHEN** `SessionHandler.SendMessage` invokes `OrchestratorRunner.Handle`
- **THEN** the call includes an `[]agent.Message` argument containing all prior user and assistant messages plus the current user message
- **AND THEN** it no longer accepts only a single `userMessage string`

#### Scenario: Confucius first LLM request includes history
- **WHEN** `Confucius.Run` receives the reconstructed history
- **THEN** its first `LLMRequest.Messages` consists of the system prompt followed by the full history ending with the current user message

#### Scenario: Sub-agent context remains isolated
- **WHEN** Confucius dispatches a sub-agent via `invoke_chongzhi` or `invoke_liang`
- **THEN** the sub-agent's `Run` still receives only its own system prompt plus a single user message assembled from the task and context arguments
- **AND THEN** the sub-agent does not see the user's full conversation history
