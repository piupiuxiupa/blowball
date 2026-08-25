## MODIFIED Requirements

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
