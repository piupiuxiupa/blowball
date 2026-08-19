## ADDED Requirements

### Requirement: Sub-agent invocation run identity
系统 SHALL 为每次子 agent 调用（invoke_chongzhi / invoke_liang）赋予唯一的 run 身份，取值为该次调用的 tool_call id，并在该次 Run 产生的全部流事件（agent_start、token、reasoning、tool_call、tool_result、agent_error、agent_end）的 `Meta.parent_tool_call_id` 中携带。Confucius 自身的事件与用户消息行不携带该字段。

#### Scenario: All sub-agent events carry the invocation id
- **WHEN** Confucius 派发一次 `invoke_chongzhi`（tool_call id 为 X），Chongzhi 在该次 Run 中发出任意流事件
- **THEN** 每个事件的 `Meta.parent_tool_call_id` 均为 X
- **AND THEN** Chongzhi 的代码不感知该字段（由分发层注入）

#### Scenario: Top-level events carry no run identity
- **WHEN** Confucius 自身输出 token 或发出 registry 工具的 tool_call/tool_result
- **THEN** 这些事件的 `Meta` 不含 `parent_tool_call_id`

### Requirement: Concurrent same-name invocations are distinguishable end-to-end
当同一回合并行派发多个同名子 agent 调用时，系统 SHALL 保证各调用的输出从 SSE 流、持久化到展示分段全程可区分，不同调用的文本内容不得互相拼接。

#### Scenario: Parallel same-name invocations stream as separate attributed runs
- **WHEN** Confucius 一轮并行派发 3 个 `invoke_chongzhi`（tool_call id 为 X1/X2/X3）
- **THEN** SSE 上三路事件各自携带 X1/X2/X3 的 `parent_tool_call_id`，按到达顺序传输但可按 id 归属
- **AND THEN** 任一调用的 token 片段在按 `(agent, parent_tool_call_id)` 分组后，组内顺序构成该调用的连贯输出

#### Scenario: Frontend segments route by agent plus run id
- **WHEN** 前端收到携带 `meta.parent_tool_call_id` 的子 agent 事件
- **THEN** 流式分段与历史重载分段均以 `(agent, parent_tool_call_id)` 为路由键
- **AND THEN** 同名并发调用呈现为相互独立的分段；不携带该字段的事件退化为按 agent 名路由（单 agent 回合行为不变）

### Requirement: Run identity is persisted with messages
子 agent 事件的 run 身份 SHALL 随消息行持久化到 `messages` 表（`run_id` 列），并随消息读取路径（MySQL 直读与 Redis 读缓存）往返不丢失。存量行与新回合中非子 agent 事件的 `run_id` 为 NULL，语义等同"无 run 身份"的现行为。

#### Scenario: Interleaved rows persist with per-run attribution
- **WHEN** 两个并发同名调用的 token 事件交错到达并持久化
- **THEN** 每行携带其所属调用的 `run_id`，行序仍为到达序（`msg_time`、`msg_index` 不变）

#### Scenario: Null run id is tolerated
- **WHEN** 读取 `run_id` 为 NULL 的存量消息行
- **THEN** 重建与展示路径按现行为处理（按 agent 名归属），不报错

### Requirement: History reconstruction regroups by run
历史重建（喂给 LLM 的上下文重建与前端历史读取）SHALL 按 `(agent, run_id)` 对带身份的交错行分组重放，组内保持到达序，使每次调用的输出在重建结果中恢复连贯；tool_call 与 tool_result 的配对仍按 `tool_call_id`，不受分组影响。

#### Scenario: Interleaved history regroups into coherent runs
- **WHEN** 历史中存在两个 run_id 交错持久化的 token/reasoning 行
- **THEN** 重建按 run_id 分组后，每个 run 的文本在组内按原到达序连贯拼接
- **AND THEN** 交错存储的行序本身不被改写（`msg_index` 与确定性 `client_msg_id` 幂等体系不变）

### Requirement: Per-invocation sub-agent instances
每次子 agent 调用 SHALL 使用独立的子 agent 实例（或等价的按调用隔离的 run 状态），并行同名调用之间不得共享可变 run 状态（副作用执行标志、round-cap 标志）。构建子 agent 所需的只读部分（配置、LLM client、工具注册表、per-turn MCP manager）MAY 共享。

#### Scenario: Side-effect flags do not cross invocations
- **WHEN** 调用 X1 的 Chongzhi 已成功执行工具，并发调用 X2 的 Chongzhi 尚未执行任何工具且随后因瞬时错误失败
- **THEN** X2 的重试资格判定只看 X2 自身的副作用标志（可重试），不被 X1 的状态影响

#### Scenario: Round-cap propagation is per invocation
- **WHEN** 并发调用中仅 X1 命中 max_rounds 上限
- **THEN** 只有 X1 的结果向 Confucius 传播 `round_capped`，X2 的结果不携带

#### Scenario: Single invocation turns are behavior-preserving
- **WHEN** 一个回合只派发一个子 agent 调用
- **THEN** 事件流、持久化与展示行为与本变更前一致（仅新增可选的 `parent_tool_call_id` 键）
