## MODIFIED Requirements

### Requirement: Sub-agent run listing API
系统 SHALL 提供需要鉴权的 `GET /api/v1/sessions/:session_id/subagents/:agent_instance_id/runs`，仅当 session 属于当前用户且实例存在时返回该实例的 run 元数据列表。列表 SHALL 按 `run_no` 升序排列，并包含 `run_id`、`previous_run_id`、`run_no`、`status`、起止时间与 `parent_instance_id`；`parent_instance_id` SHALL 等于该 run 派发时的父实例 id，根派生（由主 Agent 直接 spawn）SHALL 省略该字段。SHALL NOT 在列表响应中返回任何消息内容或内部快照 JSON。

#### Scenario: List runs for owned instance
- **WHEN** 当前用户请求自己 session 中某个实例的 run 列表
- **THEN** 系统返回 HTTP 200 与按执行顺序排列的 run 元数据

#### Scenario: List omits message payloads
- **WHEN** run 列表包含多个长输出执行
- **THEN** 响应不包含 `messages_json`、reasoning 正文或 assistant/tool 内容

#### Scenario: Cross-user instance hidden
- **WHEN** 请求的 session 不存在或不属于当前用户
- **THEN** 系统返回 HTTP 404，且不泄露目标 session 或实例是否存在

#### Scenario: Nested run carries parent identity
- **WHEN** 列表的实例本身由另一个子 Agent 派生
- **THEN** 每个 run 元数据项携带 `parent_instance_id`，值等于其父实例 id

#### Scenario: Root dispatch omits parent identity
- **WHEN** 列表的实例由主 Agent 直接派生
- **THEN** run 元数据项省略 `parent_instance_id`

### Requirement: Safe transcript DTO
run 详情 API SHALL 返回显式的前端 transcript DTO，而不是内部 OpenAI chat 消息数组。DTO SHALL 将 user 消息映射为 task、assistant 消息映射为 assistant、tool 消息映射为 tool result，并保留 `tool_call_id` 配对与消息顺序。system 消息与未知 role SHALL NOT 出现在响应中。API SHALL NOT 暴露 `messages_json`、`system_prompt`、`tools_json`、`resume_eligible` 或内部计数器。详情响应 SHALL 携带 `parent_instance_id`（根派生省略）。当 transcript 中的 assistant tool call 为 `spawn_subagent` 且同一 session 中存在 `parent_instance_id` 等于该实例、`run_id` 等于该 tool call id 的终态子 run 行时，DTO SHALL 在该 tool call 项上携带 `sub_agent_instance_id` 与 `sub_agent_name`；其余情况 SHALL 省略这两个字段。

#### Scenario: Internal system prompt omitted
- **WHEN** run 的模型上下文包含 system prompt
- **THEN** 详情响应不包含该 system prompt 或其字段

#### Scenario: Tool timeline preserved
- **WHEN** run delta 包含 assistant tool call 与对应 tool result
- **THEN** transcript 按原顺序返回 assistant 项和 tool result 项，并可按 `tool_call_id` 关联

#### Scenario: Raw snapshot fields rejected
- **WHEN** 序列化 run 详情响应
- **THEN** 响应结构不包含原始 `messages_json`、`tools_json` 或 resume 元数据字段

#### Scenario: Nested spawn tool call links its child instance
- **WHEN** 子 Agent 的 run transcript 包含一次成功的 `spawn_subagent` tool call，且孙 Agent 已写入终态 run 行
- **THEN** 该 tool call 项携带孙实例的 `sub_agent_instance_id` 与 `sub_agent_name`，前端可用该 id 调用既有 run 列表/详情 API 懒加载下一层

#### Scenario: Failed or unknown spawn carries no link
- **WHEN** transcript 中的 `spawn_subagent` tool call 因 bad_args/预算失败未创建实例，或同 session 中不存在匹配的终态子 run 行
- **THEN** 该 tool call 项省略 `sub_agent_instance_id` 与 `sub_agent_name`，响应其余部分不受影响

#### Scenario: Child link never crosses sessions
- **WHEN** 组装某 run 详情的子实例链接
- **THEN** 子 run 匹配同时约束 session、父实例 id 与 tool call id，且仅在 session 归属校验通过后执行
