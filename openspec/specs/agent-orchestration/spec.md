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

子 Agent 执行失败且被放弃（不重试、重试耗尽或取消）时，返回给 Confucius 的 tool result SHALL 携带「错误文本 + 已积累的部分输出」：子 Agent `Run` 在错误返回值中提供部分输出（失败轮已流出的 assistant 文本，或 continuation 耗尽时跨尝试累积的部分内容）且该内容非空白时，tool result content SHALL 为错误文本与部分输出的拼接（错误文本在前，以固定标记行分隔）；部分输出为空白时，tool result content SHALL 与错误文本字节级一致。失败结果 SHALL 保持 `isError` 语义且不套用 tool-result-envelope。

#### Scenario: Confucius receives function call
- **WHEN** OpenAI 返回包含 tool_calls 的响应，function name 为 invoke_chongzhi 或 invoke_liang
- **THEN** 系统解析 parameters 中的 task 和 context，启动对应子 Agent

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
- **THEN** Confucius 上下文中的 invoke tool_call/tool_result 对携带同一合并文本；子 agent 名下的事件行不参与历史重建（既有约定不变）

### Requirement: Parallel agent execution
系统 SHALL 支持 LLM 自主决定的并行 Agent 调用。当 Confucius 的 LLM 响应包含多个 tool_calls 时，并行执行。每个 tool_call 对应的子 agent 调用 SHALL 使用相互独立的实例（或等价的按调用隔离的 run 状态）；并行同名调用之间不得共享可变 run 状态（副作用执行标志、round-cap 标志）。每次调用的全部流事件 SHALL 携带该次调用的 tool_call id 作为 run 身份（`Meta.parent_tool_call_id`）。

#### Scenario: Multiple tool calls executed in parallel
- **WHEN** OpenAI 返回包含 2 个以上 tool_calls 的响应
- **THEN** 系统使用 errgroup 并行启动所有子 Agent goroutine，且每个 tool_call 获得独立的子 agent 实例

#### Scenario: Parallel same-name invocations stay isolated
- **WHEN** 并行的多个 tool_calls 解析为同名子 agent（如 3 个 `invoke_chongzhi`）
- **THEN** 各调用的 Run 状态互不可见——一个调用的工具执行不得置位其他调用的副作用标志，一个调用的 round-cap 不得污染其他调用的传播

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

### Requirement: Confucius agent loop
Confucius SHALL 实现多轮 tool-calling 循环，循环在 LLM 返回 finish_reason 为 stop 时自然终止；当循环用尽配置的 `max_rounds` 上限仍未自然终止时，SHALL 按「Agent tool-calling loop round cap and graceful termination」受控终止（WARN 日志、`usage.meta.round_capped`、一次 tool-disabled 收尾回合；详见该需求）。当某回合返回 `finish_reason=length` 时，SHALL 按 `llm-length-continuation` 能力处理：能力启用时不再将 `length` 视为循环终止信号，而是保留已输出内容并以扩容预算续写（续写尝试不消耗 `max_rounds`；耗尽时的 `length_exhausted` 终止见该能力）；能力未启用时维持 `length` 静默终止的现状行为。同一续写规则 SHALL 等价作用于 Chongzhi 与 Liang 的 tool-calling 循环及各 agent 的 tool-disabled 收尾回合。

#### Scenario: Confucius calls tools then summarizes
- **WHEN** Confucius 首轮调用返回 tool_calls，执行后第二轮 LLM 返回 content 且 finish_reason 为 stop
- **THEN** Confucius 输出最终汇总内容，推送 done 事件

#### Scenario: Confucius handles directly
- **WHEN** Confucius 首轮调用直接返回 content 且 finish_reason 为 stop（无 tool_calls）
- **THEN** Confucius 直接输出内容，推送 done 事件

#### Scenario: length round continues instead of terminating
- **WHEN** `openai.length_continue` 已启用，Confucius（或 Chongzhi/Liang）某回合返回 `finish_reason=length`
- **THEN** 该回合按 `llm-length-continuation` 能力续写（保留已输出内容、扩容预算重发），不终止循环、不消耗 `max_rounds`

#### Scenario: length still terminates when capability disabled
- **WHEN** `openai.length_continue` 未启用，某回合返回 `finish_reason=length`
- **THEN** 循环按改动前行为终止（含 `length` 携带 tool_calls 时 `shouldDispatchToolCalls` 的 WARN + terminal 处理），不追加任何续写脚手架

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
配置了 `output_schema` 的子 agent SHALL 在其 tool-calling 循环的终轮（模型不再 emit tool_call、即将返回最终内容给父级时）启用 OpenAI structured output（`response_format: json_schema`），使返回给父级的内容符合声明 schema。未配置 `output_schema` 的子 agent SHALL 返回自由文本。结构化输出与 reasoning 互斥，该冲突 SHALL 在到达 LLM 之前被拦截：部署默认等级 ≠ none 时配置加载失败（见 agent-reasoning-configuration）；请求解析结果 effort ≠ none 时返回 400（见 per-request-model-selection）。

#### Scenario: Sub-agent with output schema uses structured output on final round
- **WHEN** 一个配置了 `output_schema` 的子 agent（如 Liang）在其循环中进入终轮（`finish_reason=stop`，无 tool_call）
- **THEN** 该终轮的 LLM 请求携带 `response_format: json_schema`（内容为配置的 schema）
- **AND THEN** 子 agent 返回给父级的内容（toolResult.content）是该 schema 的合规 JSON

#### Scenario: Intermediate tool rounds not forced to structured output
- **WHEN** 配置了 `output_schema` 的子 agent 在中间轮仍需调用工具（emit tool_call）
- **THEN** 这些中间轮的 LLM 请求不携带 `response_format`，避免与 tool_call 冲突

#### Scenario: Reasoning turn never reaches a structured-output agent
- **WHEN** 某配置了 `output_schema` 的 agent 存在,且部署默认等级为非 `none`,或请求解析结果 effort ≠ none
- **THEN** 前者使配置加载失败、后者返回 HTTP 400,任何 `output_schema` agent 的 LLM 调用都不会以 effort ≠ none 执行

#### Scenario: Sub-agent without output schema returns free text
- **WHEN** 一个未配置 `output_schema` 的子 agent（如 Chongzhi）完成执行
- **THEN** 其返回内容为自由文本，行为与变更前一致

#### Scenario: Parent informed of structured return shape
- **WHEN** Confucius 的工具列表中包含一个会返回结构化 JSON 的子 agent invoke 工具
- **THEN** Confucius 的系统 prompt 声明该子 agent 返回结构化 JSON，辅助其综合质量

### Requirement: Transient error retry for sub-agent dispatch
系统 SHALL 对子 agent dispatch 的瞬时错误（LLM 429/5xx/超时、瞬时 tool_error）执行受限重试，并对语义错误（`bad_args`、`unknown_tool`）永不重试。重试策略 SHALL 由 `agents.<name>.retry` 配置块统一承载，同时治理该 agent 的轮次级重试（见 `llm-round-retry`）与派发级重试；有副作用的子 agent（Chongzhi）的派发级重试 SHALL 仅当**该次调用**尚未触发任何成功 tool_call 时允许——副作用安全由该幂等闸门承载，与 enabled 开关无关，三个 agent 的策略 SHALL 默认启用（`enabled: true`，`max_attempts: 3`）。副作用判定 SHALL 以单次调用为作用域——并行同名调用之间不得串读彼此的副作用标志。派发级重试 SHALL 与轮次级重试嵌套组合：子代理内部轮次级重试耗尽后 Run 失败，才进入派发级判定；两级重试 SHALL 共用同一 turn 级重试预算。

#### Scenario: Transient LLM error retried for read-only agent
- **WHEN** Liang 的 LLM 调用返回 429 或超时（且其轮次级重试已耗尽）
- **THEN** 系统按指数退避重试（至多配置的 max_attempts，默认 3），复用相同 invoke 参数
- **AND THEN** 重试时发射 `agent_error` 事件且 `Meta.retry=true` 以通知前端

#### Scenario: Semantic error never retried
- **WHEN** 子 agent dispatch 因 `bad_args` 或 `unknown_tool` 失败
- **THEN** 系统立即返回错误结果，不重试（相同参数必再败）

#### Scenario: Side-effecting agent not retried after tool execution
- **WHEN** Chongzhi 已成功执行至少一个 xizhi 工具调用（如 write_file）后，其后续 LLM 调用失败
- **THEN** 系统不重试该 dispatch（已产生文件系统副作用，重试将重复执行）
- **AND THEN** 错误结果喂回 Confucius 决策

#### Scenario: Side-effect scope is per invocation under concurrency
- **WHEN** 并行同名调用中，调用 X1 已成功执行工具，调用 X2 尚未执行任何工具且 X2 因瞬时错误失败
- **THEN** X2 按自身状态判定为可重试，X1 的副作用不得抑制 X2 的重试

#### Scenario: Side-effecting agent retried only before any tool call
- **WHEN** Chongzhi 首轮 LLM 调用即失败（尚未触发任何 tool_call，含轮次级重试耗尽的情形）
- **THEN** 系统可重试该 dispatch（无副作用风险，幂等闸门放行），重试上限受重试预算约束

#### Scenario: Retry budget enforced
- **WHEN** 一个 turn 内累计重试消耗的 token 达到配置预算上限
- **THEN** 系统停止后续重试（派发级与轮次级一致），将当前错误结果喂回 Confucius（依赖 `turn-cost-tracking` 数据防失控）

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

### Requirement: Agent tool-calling loop round cap and graceful termination
每个 Agent（Confucius、Chongzhi、Liang）的 tool-calling 循环 SHALL 受该 Agent 配置的 `max_rounds` 上限约束。当循环用尽 `max_rounds` 轮仍未自然终止（模型持续 emit tool_calls）时，系统 SHALL：(a) 发射一条结构化 WARN 日志（含 agent 名、上限值、已执行轮数）；(b) 将 `usage.meta.round_capped` 置为 `true` 折入终态 done 事件（非 cap 终止的 turn SHALL 不携带该键）；(c) 执行一次"收尾回合"——发起一轮额外 LLM 请求且禁用 tools（Confucius 同时不再 dispatch 子 agent），并在消息末尾追加一条收尾指令（告知模型已达轮数上限、不得再调用工具、须基于既有对话给出最终答复），以引导模型综合而非继续 tool-use 流；该轮 token SHALL 正常计入用量。若收尾回合返回 finish_reason=stop 的可用文本内容（无 tool_calls），其内容成为 turn 的 `finalContent`，turn 按正常成功结束（WARN 日志与 `meta.round_capped` 为仅有的 cap 信号，不发射 `agent_error`，done 事件不携带 `error`）。若收尾回合返回 tool_calls（某些 OpenAI 兼容网关在 tools 缺省时仍回显习得的 tool-use，这些调用无法被受理、其伴随文本仅为"接下来调用 X"式前言而非综合答案）、或未产出内容、或出错，均视为收尾失败：系统 SHALL 发射 `agent_error`（`Meta.error_code` 为 `round_cap_exhausted`）随后 `agent_end`，done 事件 SHALL 携带 `error` 字段，且该前言文本 SHALL NOT 作为最终答案返回。自然终止（`finish_reason=stop`、空 assistant 响应）SHALL 不触发上述任何 cap 行为。

#### Scenario: Cap hit with successful wrap-up round
- **WHEN** 某 Agent 的循环用尽 `max_rounds` 轮仍未自然终止，且随后的 tool-disabled 收尾回合返回了非空 content
- **THEN** 该 content 成为 turn 的最终输出，done 事件的 `usage.meta.round_capped` 为 `true`
- **AND THEN** 系统不发射 `agent_error`，done 事件不携带 `error` 字段

#### Scenario: Cap hit with empty wrap-up round surfaces error
- **WHEN** 某 Agent 的循环用尽 `max_rounds` 轮仍未自然终止，且随后的收尾回合未返回可用内容（空 content 或 LLM 错误）
- **THEN** 系统发射 `agent_error`（`Meta.error_code` 为 `round_cap_exhausted`）随后 `agent_end`
- **AND THEN** done 事件携带 `error` 字段，`usage.meta.round_capped` 为 `true`

#### Scenario: Cap hit where wrap-up emits tool_calls surfaces error
- **WHEN** 收尾回合的 LLM 响应携带 tool_calls（即便请求未携带 tools，某些 OpenAI 兼容网关仍会回显习得的 tool-use）
- **THEN** 系统 SHALL 视为收尾失败：发射 `agent_error`（`Meta.error_code` 为 `round_cap_exhausted`）随后 `agent_end`，done 事件携带 `error` 字段
- **AND THEN** 该响应的伴随文本 SHALL NOT 作为 turn 的最终内容返回

#### Scenario: Wrap-up round disables tools
- **WHEN** 收尾回合的 LLM 请求被构造
- **THEN** 该请求 SHALL 不携带 `tools`（且 Confucius 在该回合不 dispatch 子 agent），使模型只能返回最终文本

#### Scenario: Wrap-up round counts usage normally
- **WHEN** 收尾回合产生 token 用量
- **THEN** 该用量 SHALL 折入该 Agent 的 per-agent 与 turn 总用量，与正常回合一致

#### Scenario: Reasoning agent wrap-up round
- **WHEN** 一个运行在 reasoning wire 家族的 Agent（该 turn 解析到 `thinking: true` 的目录条目）触达上限并执行收尾回合
- **THEN** 收尾回合 SHALL 正常产出 reasoning + content，不发生 response_format 冲突

#### Scenario: Structured-output agent wrap-up round
- **WHEN** 一个配置了 `output_schema` 的子 agent（如 Liang）触达上限并执行收尾回合
- **THEN** 该收尾回合作为终轮 SHALL 携带 `response_format: json_schema`，返回内容为该 schema 的合规 JSON

#### Scenario: Configurable cap respected
- **WHEN** 某 Agent 配置 `max_rounds: 3`，模型连续 3 轮 emit tool_calls
- **THEN** 循环在第 3 轮后进入收尾回合（不再执行第 4 个 tool-calling 轮）

#### Scenario: Default cap applied when unset
- **WHEN** 某 Agent 未配置 `max_rounds`
- **THEN** 循环上限 SHALL 为默认值 `100`

#### Scenario: Natural termination emits no cap signal
- **WHEN** 某 Agent 在用尽 `max_rounds` 之前以 `finish_reason=stop` 或空 assistant 响应自然终止
- **THEN** 系统 SHALL 不发射 WARN cap 日志、不设置 `meta.round_capped`、不发射 `round_cap_exhausted`，done 事件不携带 `error`

#### Scenario: Warn log emitted on every cap hit
- **WHEN** 任一 Agent 的循环因用尽 `max_rounds` 进入收尾路径
- **THEN** 系统 SHALL 发射一条结构化 WARN 日志，包含 agent 名、`max_rounds` 值与已执行轮数
