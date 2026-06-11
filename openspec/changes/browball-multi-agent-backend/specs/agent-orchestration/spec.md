## ADDED Requirements

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
每个 Agent 的 system_prompt、model、max_tokens、tools 列表 SHALL 从 config.yaml 加载。

#### Scenario: Load agent config on startup
- **WHEN** 服务启动
- **THEN** 系统从 config.yaml 的 agents 段加载所有 Agent 配置，构建 Agent 实例

#### Scenario: Configurable tool permissions
- **WHEN** Agent 配置中 tools 列表为空
- **THEN** 该 Agent 调用 OpenAI 时不传递 tools 参数

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
