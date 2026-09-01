## MODIFIED Requirements

### Requirement: Usage JSON shape is authoritative
`turn_usage.usage_json` 与 done 事件 `Meta.usage` SHALL 持有相同的对象形状，作为 per-agent 成本归因的权威定义。形状为：`{total: {prompt_tokens, completion_tokens, total_tokens, reasoning_tokens?}, by_agent: {<agent-label>: {...}}, meta: {sub_agent_invocations: [...], parallel: bool}}`。`by_agent` 的 key SHALL 为动态实例标签（实例 id 或 `name` 参数派生的展示名），跨续跑的同一实例 SHALL 累计到同一 key；`sub_agent_invocations` 的每一项 SHALL 记录实例 id、父实例（根派发为空）与派发顺序。

#### Scenario: Shape matches done event exactly
- **WHEN** 系统构造 turn 的 usage 对象
- **THEN** 落库的 `usage_json` 与 done 事件 `Meta.usage` 是同一个对象的不同序列化（内容一致）

#### Scenario: Reasoning tokens included when present
- **WHEN** 某 agent 产生了 reasoning tokens（`thinking: true`）
- **THEN** 该 agent 在 `by_agent` 下的对象包含 `reasoning_tokens` 字段，`total` 也包含聚合的 `reasoning_tokens`

#### Scenario: Parallelism metadata recorded
- **WHEN** 一个 turn 中存在某个 assistant 轮次同时调度了 ≥2 个 tool_call
- **THEN** `usage_json.meta.parallel` 为 `true`
- **AND THEN** `usage_json.meta.sub_agent_invocations` 按派发顺序列出全部 spawn 派发，每项含实例 id 与父实例

#### Scenario: Resumed instance accumulates into one entry
- **WHEN** 同一子 Agent 实例在本 turn 内被派发两次（初始 + 续跑）
- **THEN** 其两次执行的 usage 累计到 `by_agent` 的同一 key 下
