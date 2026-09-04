# subagent-run-identity Specification

## Purpose

每次子 agent 调用（invoke_chongzhi / invoke_liang）拥有唯一的 run 身份（取值为该次调用的 tool_call id），贯穿全程：流事件以 `Meta.parent_tool_call_id` 携带身份，消息行以 `messages.run_id` 持久化，历史重建与前端分段按 `(agent, run_id)` 归属，使并行同名调用的交错输出互不拼接、各自连贯。同时每次调用使用相互独立的子 agent 实例（或等价的按调用隔离的 run 状态），并行同名调用之间不共享可变 run 状态（副作用执行标志、round-cap 标志）；不携带身份的事件退化为按 agent 名归属，单调用回合保持行为不变。

## Requirements

### Requirement: Sub-agent invocation run identity
系统 SHALL 为每个子 Agent 实例分配稳定的 `agent_instance_id`（跨续跑不变，作为实例的长期身份），并为每次派发（含续跑）分配独立的 run 身份（取值为该次 tool_call id）。该次 Run 产生的全部流事件（agent_start、token、reasoning、tool_call、tool_result、agent_error、agent_end）的 `Meta` SHALL 同时携带 `agent_instance_id` 与 `parent_tool_call_id`（run 身份）。Confucius 自身的事件与用户消息行不携带这两个字段。实例身份由派发层分配与注入，子 Agent 执行体 SHALL NOT 感知。

#### Scenario: All sub-agent events carry the invocation id
- **WHEN** Confucius 派发一次 spawn_subagent（tool_call id 为 X，实例 id 为 I），子 Agent 在该次 Run 中发出任意流事件
- **THEN** 每个事件的 `Meta.agent_instance_id` 为 I 且 `Meta.parent_tool_call_id` 为 X
- **AND THEN** 子 Agent 的代码不感知这两个字段（由分发层注入）

#### Scenario: Instance id survives resume
- **WHEN** 实例 I 被续跑（新 tool_call id 为 Y）
- **THEN** 续跑全部事件携带 `agent_instance_id: I` 与 `parent_tool_call_id: Y`

#### Scenario: Top-level events carry no run identity
- **WHEN** Confucius 自身输出 token 或发出 registry 工具的 tool_call/tool_result
- **THEN** 这些事件的 `Meta` 不含 `agent_instance_id` 与 `parent_tool_call_id`

### Requirement: Concurrent same-name invocations are distinguishable end-to-end
当同一回合并行派发多个子 Agent 调用时，系统 SHALL 保证各调用的输出从 SSE 流、持久化到展示分段全程可区分，不同调用的文本内容不得互相拼接。前端 SHALL 以 `(agent, agent_instance_id)` 为线程聚合键（一个实例连同其全部续跑呈现为一条子 Agent 线程），并以 `parent_tool_call_id` 区分实例内的每次执行。

#### Scenario: Parallel same-name invocations stream as separate attributed runs
- **WHEN** Confucius 一轮并行派发 3 个 spawn_subagent（tool_call id 为 X1/X2/X3，实例 id 为 I1/I2/I3）
- **THEN** SSE 上三路事件各自携带对应实例与 run 身份，按到达顺序传输但可按 id 归属
- **AND THEN** 任一调用的 token 片段在按 `(agent, agent_instance_id)` 分组后，组内顺序构成该调用的连贯输出

#### Scenario: Frontend segments route by agent plus run id
- **WHEN** 前端收到携带 `meta.agent_instance_id` 的子 Agent 事件
- **THEN** 流式分段与历史重载分段以 `(agent, agent_instance_id)` 为线程路由键，`parent_tool_call_id` 用于实例内执行分段
- **AND THEN** 同名并发调用呈现为相互独立的线程；不携带该字段的事件退化为按 agent 名路由（存量行为不变）

### Requirement: Run identity is persisted with messages
子 Agent 事件的实例身份与 run 身份 SHALL 随消息行持久化到 `messages` 表（`agent_instance_id` 与 `run_id` 列），并随消息读取路径（MySQL 直读与 Redis 读缓存）往返不丢失。存量行与新回合中非子 agent 事件的两个字段为 NULL，语义等同"无身份"的现行为。

#### Scenario: Interleaved rows persist with per-run attribution
- **WHEN** 两个并发实例的 token 事件交错到达并持久化
- **THEN** 每行携带其所属实例的 `agent_instance_id` 与该次派发的 `run_id`，行序仍为到达序（`msg_time`、`msg_index` 不变）

#### Scenario: Null run id is tolerated
- **WHEN** 读取 `agent_instance_id` / `run_id` 为 NULL 的存量消息行
- **THEN** 重建与展示路径按现行为处理（按 agent 名归属），不报错

### Requirement: History reconstruction regroups by run
历史重建（前端历史读取）SHALL 按 `(agent, agent_instance_id)` 对带身份的交错行分组重放，组内保持到达序，使每个实例的输出（含多次续跑）在重建结果中恢复为连贯线程；tool_call 与 tool_result 的配对仍按 `tool_call_id`，不受分组影响。喂给 LLM 的主对话上下文重建不受影响：子 agent 名下的事件行仍不参与主上下文重建。

#### Scenario: Interleaved history regroups into coherent runs
- **WHEN** 历史中存在多个实例交错持久化的 token/reasoning 行
- **THEN** 重建按 `agent_instance_id` 分组后，每个实例的文本在组内按原到达序连贯拼接，续跑内容并入同一线程
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
