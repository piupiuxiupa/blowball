## MODIFIED Requirements

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

### Requirement: Transient error retry for sub-agent dispatch
系统 SHALL 对子 agent dispatch 的瞬时错误（LLM 429/5xx/超时、瞬时 tool_error）执行受限重试，并对语义错误（`bad_args`、`unknown_tool`）永不重试。重试策略 SHALL 按 agent 幂等性区分：只读子 agent（Liang）默认可重试；有副作用的子 agent（Chongzhi）仅当**该次调用**尚未触发任何成功 tool_call 时可重试。副作用判定 SHALL 以单次调用为作用域——并行同名调用之间不得串读彼此的副作用标志。

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

#### Scenario: Side-effect scope is per invocation under concurrency
- **WHEN** 并行同名调用中，调用 X1 已成功执行工具，调用 X2 尚未执行任何工具且 X2 因瞬时错误失败
- **THEN** X2 按自身状态判定为可重试，X1 的副作用不得抑制 X2 的重试
