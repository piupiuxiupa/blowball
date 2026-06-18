# agent-orchestration Specification

## Purpose

定义多 Agent 系统的编排能力，包括 flat 拓扑、基于 function-calling 的调度、并行执行、上下文隔离、流式透传、配置加载、Confuse 主控循环以及 token 用量可观测性。

## Requirements

### Requirement: Flat agent topology
系统 SHALL 采用 flat 拓扑，仅 Confuse 可调度其他 Agent，子 Agent 不允许嵌套调用。

#### Scenario: Confuse dispatches sub-agents
- **WHEN** Confuse 通过 function-calling 调用 invoke_chongzhi 或 invoke_liang
- **THEN** 系统启动对应子 Agent 执行任务

#### Scenario: Sub-agents cannot call other agents
- **WHEN** Chongzhi 或 Liang 的 tool list 被构建
- **THEN** tool list 中不包含 invoke_chongzhi、invoke_liang 等其他 Agent 调度工具

### Requirement: Agent as tool via function calling
Confuse SHALL 通过 OpenAI function-calling 机制调度子 Agent，每个子 Agent 定义为一个 tool。

#### Scenario: Confuse receives function call
- **WHEN** OpenAI 返回包含 tool_calls 的响应，function name 为 invoke_chongzhi 或 invoke_liang
- **THEN** 系统解析 parameters 中的 task 和 context，启动对应子 Agent

#### Scenario: Tool call result returned to Confuse
- **WHEN** 子 Agent 执行完成（成功或失败）
- **THEN** 结果作为 tool role message 追加到 Confuse 的消息列表，Confuse 进行下一轮决策

### Requirement: Parallel agent execution
系统 SHALL 支持 LLM 自主决定的并行 Agent 调用。当 Confuse 的 LLM 响应包含多个 tool_calls 时，并行执行。

#### Scenario: Multiple tool calls executed in parallel
- **WHEN** OpenAI 返回包含 2 个以上 tool_calls 的响应
- **THEN** 系统使用 errgroup 并行启动所有子 Agent goroutine

#### Scenario: Parallel results collected
- **WHEN** 所有并行子 Agent 执行完成
- **THEN** 系统按 tool_call_id 对应关系收集所有结果，构造 tool response messages

#### Scenario: One agent fails in parallel execution
- **WHEN** 并行执行中一个子 Agent 失败
- **THEN** 失败信息作为错误 StreamEvent 流式通知，其他 Agent 继续执行，失败结果返回 Confuse 决策

### Requirement: Independent agent context
子 Agent SHALL 在独立上下文中运行，只接收 Confuse 传递的 task description 和 context。

#### Scenario: Sub-agent receives isolated context
- **WHEN** Confuse 调用子 Agent
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
每个 Agent 的 system_prompt、model、max_tokens、tools 列表、mcp 配置及 skills 配置 SHALL 从 config.yaml 加载，其中 tools 列表中的名称可以解析为内置工具或已通过 MCP client 注册的外部 MCP 代理工具。

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

### Requirement: Confuse agent loop
Confuse SHALL 实现多轮 tool-calling 循环，直到 LLM 返回 finish_reason 为 stop。

#### Scenario: Confuse calls tools then summarizes
- **WHEN** Confuse 首轮调用返回 tool_calls，执行后第二轮 LLM 返回 content 且 finish_reason 为 stop
- **THEN** Confuse 输出最终汇总内容，推送 done 事件

#### Scenario: Confuse handles directly
- **WHEN** Confuse 首轮调用直接返回 content 且 finish_reason 为 stop（无 tool_calls）
- **THEN** Confuse 直接输出内容，推送 done 事件

### Requirement: Token usage observability
系统 SHALL 记录每次请求中每个 Agent 的 token 用量，在 done 事件中汇总。

#### Scenario: Token usage in done event
- **WHEN** 一次完整的用户请求处理完成
- **THEN** StreamEvent{Type: "done"} 的 Meta 包含 total_tokens 和各 agent 的 token 明细（prompt_tokens, completion_tokens）

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
The orchestrator SHALL accept the complete session conversation history recovered from persistence, combined with the current user message, and pass it to the Confuse agent loop as the initial `messages` slice.

#### Scenario: OrchestratorRunner.Handle signature carries history
- **WHEN** `SessionHandler.SendMessage` invokes `OrchestratorRunner.Handle`
- **THEN** the call includes an `[]agent.Message` argument containing all prior user and assistant messages plus the current user message
- **AND THEN** it no longer accepts only a single `userMessage string`

#### Scenario: Confuse first LLM request includes history
- **WHEN** `Confuse.Run` receives the reconstructed history
- **THEN** its first `LLMRequest.Messages` consists of the system prompt followed by the full history ending with the current user message

#### Scenario: Sub-agent context remains isolated
- **WHEN** Confuse dispatches a sub-agent via `invoke_chongzhi` or `invoke_liang`
- **THEN** the sub-agent's `Run` still receives only its own system prompt plus a single user message assembled from the task and context arguments
- **AND THEN** the sub-agent does not see the user's full conversation history
