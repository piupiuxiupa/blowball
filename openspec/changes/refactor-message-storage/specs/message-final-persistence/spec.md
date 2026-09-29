## ADDED Requirements

### Requirement: Round-boundary final content persistence
系统 SHALL 以 round 为最小持久化单位：一轮（一次完整 LLM 回复及其全部工具执行结果）完成后，将该 round 的终稿写入 MySQL。持久化批 SHALL 在单事务内跨 `messages` 与 `tool_calls` 两表写入。新数据 SHALL NOT 产生 `token`、`reasoning`、`tool_result`、`agent_start`、`agent_end` 或 `plan_updated` 事件行；例外：`agent_error` SHALL 作为错误留痕行持久化（role 为空、content 为错误文本、随所属 round 批次落库），`tool_call` 仅以占位行形式存在（见下）。

#### Scenario: 一轮完成即落库
- **WHEN** 主 agent 完成 round 1（assistant 回复 + 2 个工具调用及结果）后开始 round 2
- **THEN** round 1 的 assistant 终稿行与其 2 条 `tool_calls` 行已在单事务内写入 MySQL
- **AND** round 2 的流式 chunk 尚未产生任何数据库行

#### Scenario: 无工具的最终轮
- **WHEN** 最后一轮 assistant 回复不含工具调用，turn 正常结束
- **THEN** 该轮落库为一条 assistant 终稿行，无 tool_calls 行

### Requirement: messages stores final content and tool placeholders
`messages` 表 SHALL 存储 user 终稿行、assistant 终稿行（完整 content 与 reasoning），以及工具占位行（`event_type=tool_call`、仅携带 `tool_call_id` 与工具名、不携带参数/结果 payload）。占位行在其 `(msg_time, msg_index)` 位置锚定一次工具交互， SHALL 通过 `tool_call_id` join `tool_calls` 表取回 payload。SSE 实时流 SHALL 保持逐 chunk 推送不变。

#### Scenario: token chunk 不落库
- **WHEN** 一轮 assistant 流式输出 500 个 token chunk
- **THEN** SSE 消费者收到 500 个 token 事件
- **AND** 该轮结束后 `messages` 只有一行 assistant 终稿

### Requirement: tool_calls table is the canonical tool interaction store
`tool_calls` 表 SHALL 为工具交互的唯一权威存储：每行包含 `tool_call_id`（配对键）、`name`、`arguments`、`result`、`status` 与 `turn_id`。call 与 result SHALL 同行存储。`(session_id, tool_call_id)` SHALL 为唯一键，使重试幂等。参数与结果 payload SHALL NOT 重复存储于 `messages`。

#### Scenario: call 与 result 同行配对
- **WHEN** round 内工具 `call_abc` 执行完成
- **THEN** `tool_calls` 存在一行：`tool_call_id='call_abc'`，`arguments` 与 `result` 均已填充，`status='completed'`

#### Scenario: 重试不产生重复行
- **WHEN** round 批写因网络错误重试
- **THEN** UK 使同一 `tool_call_id` 只留一行

### Requirement: Deterministic cross-table reconstruction
对话历史重建 SHALL 按 `(msg_time, msg_index)` 顺序遍历 `messages`：assistant 终稿行直出 content/reasoning；工具占位行按 `tool_call_id` join `tool_calls`，与同轮占位行合并为一条 wire format `tool_calls` 数组的 assistant 消息，后接 `role=tool` 结果消息。重建产出的请求序列 SHALL 与既有 wire format 逐 byte 一致，以保持 LLM 前缀缓存命中。

#### Scenario: 双表合并顺序确定
- **WHEN** round 1 含 assistant 回复与并行 3 个工具调用
- **THEN** 重建序列为 user、assistant(+3 tool_calls)、tool×3（按调用序）

#### Scenario: 前缀缓存跨迁移边界连续
- **WHEN** 同一会话混合遗留 chunk 行与新终稿行
- **THEN** 重建逻辑对两种输入产出相同的请求消息序列

### Requirement: Streaming chunks live in Redis until round completion
进行中 round 的流式内容 SHALL 仅存在于 Redis（复用 run event log）；round 完成批写成功后，对应 in-flight 累积 SHALL 被清理。msgflush chunk 级全局队列与 flusher SHALL 退役；用户消息 send-time 持久化 SHALL 保留。

#### Scenario: crash 丢当前 round
- **WHEN** 进程在 round N 进行中崩溃
- **THEN** round 1..N-1 已落库可见，round N 内容丢失，无半截行进入历史

#### Scenario: cancel/error 持久化截断终稿
- **WHEN** turn 被取消或 provider 返回错误时 round N 已累积部分内容
- **THEN** round N 累积内容作为截断 assistant 终稿行落库并标注 interrupted

### Requirement: Sub-agent content is excluded from messages
持久化层 SHALL 按事件 Meta 的 `agent_instance_id` 分流：非空的事件 SHALL NOT 写入 `messages` 或 `tool_calls`，其 transcript 由既有 `subagent_runs` 快照提供。

#### Scenario: 子 agent 流不进 messages
- **WHEN** 子 agent 运行产生 token/tool 事件
- **THEN** SSE 与 `subagent_runs` 快照正常
- **AND** `messages` 与 `tool_calls` 无该子 agent 的行

### Requirement: Legacy chunk rows read dual-mode without migration
存量事件行 SHALL NOT 迁移。重建路径 SHALL 按 event type 双模：终稿行直读组装，遗留 chunk 行走既有合并逻辑，二者产出一致的请求序列。

#### Scenario: 旧会话照常重建
- **WHEN** 会话历史全部为迁移前的 chunk 行
- **THEN** prompt 重建与迁移前行为一致
