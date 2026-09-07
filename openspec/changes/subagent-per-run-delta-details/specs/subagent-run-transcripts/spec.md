## ADDED Requirements

### Requirement: Per-run delta persistence
系统 SHALL 为每个已终止的子 Agent run 持久化一行 run 记录，并以 `(session_id, run_id)` 唯一标识。run 行的 `messages_json` SHALL 只包含本次执行新增的 user、assistant 与 tool 消息，MUST NOT 重复保存先前 run 的上下文或实例 system prompt。同一 dispatch 的重试 SHALL 复用同一 run 记录并以最终终态更新该记录。

#### Scenario: Initial run stores only its own messages
- **WHEN** 一个新子 Agent 实例完成初始执行
- **THEN** 系统为该实例写入 `run_no=1` 的 run 记录，其 `messages_json` 只包含本次任务、assistant 输出与工具消息

#### Scenario: Resume stores a new delta row
- **WHEN** 一个已有实例被续跑并完成新的执行
- **THEN** 系统保留旧 run 记录，并写入指向旧 run 的新 run 记录；新记录只包含本次续跑新增的消息

#### Scenario: Dispatch retry updates one run
- **WHEN** 同一个 `spawn_subagent` tool call 的子 Agent 执行发生 dispatch 级重试
- **THEN** 系统不创建新的 run 记录，而是用最终尝试的终态更新原 `(session_id, run_id)` 记录

### Requirement: Linear run-chain reconstruction
系统 SHALL 为每个子 Agent 实例维护稳定元数据、system prompt 与 `latest_run_id`，并保证该实例的 run 记录通过 `run_no` 与 `previous_run_id` 形成一条线性链。续跑 SHALL 读取并按序拼接该链上的 delta、前置实例 system prompt，得到完整模型上下文后追加新任务。链路缺失、顺序不连续、计数不一致或 JSON 不可读时，目标 SHALL 被视为不可续跑并以 bad_args 失败。

#### Scenario: Resume reconstructs full context
- **WHEN** 一个实例已有多个 delta run 且链路完整
- **THEN** 续跑加载的模型上下文等价于按顺序拼接 system prompt 与所有 run delta 后的结果

#### Scenario: Broken chain rejected
- **WHEN** 某个 run 的 `previous_run_id`、`run_no` 或 `base_message_count` 与前驱记录不一致
- **THEN** 续跑以 bad_args 失败，且不执行子 Agent LLM 调用

#### Scenario: Concurrent resume does not fork the chain
- **WHEN** 两个派发同时尝试续跑同一个实例
- **THEN** 只有一个派发能推进 `latest_run_id` 并追加后继 run；另一个派发失败，不产生分叉链

### Requirement: Sub-agent run listing API
系统 SHALL 提供需要鉴权的 `GET /api/v1/sessions/:session_id/subagents/:agent_instance_id/runs`，仅当 session 属于当前用户且实例存在时返回该实例的 run 元数据列表。列表 SHALL 按 `run_no` 升序排列，并包含 `run_id`、`previous_run_id`、`run_no`、`status` 与起止时间；SHALL NOT 在列表响应中返回任何消息内容或内部快照 JSON。

#### Scenario: List runs for owned instance
- **WHEN** 当前用户请求自己 session 中某个实例的 run 列表
- **THEN** 系统返回 HTTP 200 与按执行顺序排列的 run 元数据

#### Scenario: List omits message payloads
- **WHEN** run 列表包含多个长输出执行
- **THEN** 响应不包含 `messages_json`、reasoning 正文或 assistant/tool 内容

#### Scenario: Cross-user instance hidden
- **WHEN** 请求的 session 不存在或不属于当前用户
- **THEN** 系统返回 HTTP 404，且不泄露目标 session 或实例是否存在

### Requirement: Sub-agent run detail lazy loading
系统 SHALL 提供需要鉴权的 `GET /api/v1/sessions/:session_id/subagents/:agent_instance_id/runs/:run_id`，仅当 session 属于当前用户且 run 属于该 session 与实例时返回该 run 的 transcript。detail 响应 SHALL 基于 run 行中的 delta 消息生成，不拼接其他 run 的内容。run 尚未产生终态快照或目标不存在时 SHALL 返回 HTTP 404。

#### Scenario: Detail returns only the selected run
- **WHEN** 当前用户请求某个实例的第 2 次 run 详情
- **THEN** transcript 只包含第 2 次 dispatch 新增的任务、assistant 与 tool 消息，不重复第 1 次 run

#### Scenario: Active run has no durable detail
- **WHEN** 请求的 run 仍在执行且尚未写入终态快照
- **THEN** 系统返回 HTTP 404；实时展示继续使用既有 SSE/run stream 通道

#### Scenario: Mismatched instance and run rejected
- **WHEN** `run_id` 存在但不属于路径中的 `agent_instance_id` 或 session
- **THEN** 系统返回 HTTP 404

### Requirement: Safe transcript DTO
run 详情 API SHALL 返回显式的前端 transcript DTO，而不是内部 OpenAI chat 消息数组。DTO SHALL 将 user 消息映射为 task、assistant 消息映射为 assistant、tool 消息映射为 tool result，并保留 `tool_call_id` 配对与消息顺序。system 消息与未知 role SHALL NOT 出现在响应中。API SHALL NOT 暴露 `messages_json`、`system_prompt`、`tools_json`、`resume_eligible` 或内部计数器。

#### Scenario: Internal system prompt omitted
- **WHEN** run 的模型上下文包含 system prompt
- **THEN** 详情响应不包含该 system prompt 或其字段

#### Scenario: Tool timeline preserved
- **WHEN** run delta 包含 assistant tool call 与对应 tool result
- **THEN** transcript 按原顺序返回 assistant 项和 tool result 项，并可按 `tool_call_id` 关联

#### Scenario: Raw snapshot fields rejected
- **WHEN** 序列化 run 详情响应
- **THEN** 响应结构不包含原始 `messages_json`、`tools_json` 或 resume 元数据字段

### Requirement: Legacy full snapshot compatibility
迁移既有子 Agent 数据时，系统 SHALL 为每个旧实例保留一份可读的 legacy full snapshot 记录并标记其存储类型，且 SHALL 尽量从既有消息行回填该记录的 `run_id`。legacy full snapshot SHALL 可作为实例 run 链的基线供后续 delta 续跑；系统 MUST NOT 伪造已被旧实现覆盖的历史 run 记录。

#### Scenario: Legacy instance remains resumable
- **WHEN** 迁移前存在的实例只有一份旧完整快照
- **THEN** 系统将其标记为 legacy full snapshot，并可在后续续跑时作为基线拼接新的 delta run

#### Scenario: Overwritten history is not fabricated
- **WHEN** 旧实例曾经历多次续跑但只保留最后一份完整快照
- **THEN** 迁移后不生成虚构的早期 per-run 记录；run 列表只暴露真实保留的 legacy run 与之后的新 run
