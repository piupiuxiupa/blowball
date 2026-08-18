## RENAMED Requirements

- FROM: `### Requirement: Three-layer message storage`
- TO: `### Requirement: Redis-first message storage`

## MODIFIED Requirements

### Requirement: Redis-first message storage

消息 SHALL 按顺序写入 Redis 的两类 key：`msgs:{session_id}`（读热层，24h TTL）与 `msgs:buffer`（入库队列，无 TTL），二者在同一 pipeline 中批量 RPUSH。同一 turn 内，用户消息与 assistant 事件在 orchestrator 结束后通过同一个 goroutine 作为一批消息写入；MySQL 写入由后台 flusher 异步批量完成（见 message-write-behind 能力）；系统 SHALL NOT 再写任何文件系统会话文件。orchestrator 失败（含客户端中断）时，已产生的部分事件与该 turn 的用户消息 SHALL 照常持久化（与 interrupted-turn-persistence 能力一致）。

#### Scenario: Batch write user and assistant messages after success

- **WHEN** orchestrator 成功完成一次 turn
- **THEN** 系统 SHALL 通过 goroutine 将用户消息与全部 assistant 事件作为一批消息在同一 Redis pipeline 中双写（`RPUSH msgs:{session_id}` 刷新 TTL + `RPUSH msgs:buffer`），元素为同一份消息 JSON
- **AND THEN** batch 内消息顺序为 user message（msg_index=0）后接 assistant 事件（msg_index 从 1 严格单调递增），每条消息携带唯一 `client_msg_id`
- **AND THEN** 该批次不触发同步 MySQL 写入，也不写任何文件系统会话文件

#### Scenario: Persist partial turn on orchestrator failure

- **WHEN** orchestrator 返回错误（含上游 429/5xx、超时、客户端中断取消）
- **THEN** 系统 SHALL 照常持久化该 turn 的用户消息与已收集的部分 assistant 事件（同一 Redis 双写路径），使重载的会话历史与用户实际看到的内容一致

#### Scenario: Redis write failure

- **WHEN** Redis 双写 pipeline 失败（Redis 不可用）
- **THEN** 系统 SHALL 降级为同步直写 MySQL（幂等 INSERT），记录错误日志，不丢弃该批消息，不阻塞响应

#### Scenario: Goroutine persistence independent of request context

- **WHEN** 客户端在 SSE 流结束前断开连接（取消请求 ctx）
- **THEN** 批量入库 goroutine SHALL 使用派生自 `context.Background()` 的 detached ctx 继续完成写入（保留 trace_id），写入内容包含该 turn 的用户消息与 assistant 事件

### Requirement: Message recovery with fallback

系统 SHALL 按优先级恢复会话消息：Redis → MySQL，并按 `(msg_time, msg_index)` 升序排列。Redis 未命中时，系统 SHALL 先执行一次有界同步 drain（将入库队列中当前全部记录插入 MySQL，见 message-write-behind 的同步 drain 原语），再从 MySQL 读取并回填 Redis 缓存。文件系统不再参与恢复。

#### Scenario: Recover from Redis cache

- **WHEN** 加载会话消息且 Redis 中存在缓存
- **THEN** 系统从 Redis 读取消息列表，按 `(msg_time, msg_index)` 升序返回，不查询数据库

#### Scenario: Drain before MySQL fallback

- **WHEN** Redis 缓存未命中且入库队列中存在尚未入库的消息
- **THEN** 系统 SHALL 先同步 drain 入库队列（buffer 与 processing），确保这些消息进入 MySQL 后再执行 MySQL 读取，避免回填覆盖未入库消息

#### Scenario: Final fallback to MySQL

- **WHEN** Redis 缓存未命中（且 drain 已完成）
- **THEN** 系统从 MySQL messages 表查询（`ORDER BY msg_time ASC, msg_index ASC`）并回填 Redis 缓存（`SetMessages`）

#### Scenario: Recovery returns full event stream

- **WHEN** 系统恢复一个已存在的会话历史
- **THEN** 返回的 message 序列 SHALL 包含每个 turn 的用户消息以及 assistant 在该 turn 内产生的所有事件（token / tool_call / agent_start / agent_end / agent_error），按产生顺序排列

### Requirement: Message data model

每条消息 SHALL 包含 session_id、msg_time、agent、msg_index、role、event_type、content、trace_id、client_msg_id。`event_type` 标识消息的语义类别，`agent` 标识消息的产生方（用户消息填 `'user'`，assistant 事件填产生该事件的 agent 名），`client_msg_id` 为持久化时 mint 的幂等键（UUID）。

#### Scenario: Message record structure

- **WHEN** 消息写入 MySQL
- **THEN** messages 表包含字段：id (BIGINT AUTO_INCREMENT)、session_id (CHAR 36)、msg_time (TIMESTAMP(3) 毫秒精度)、agent (VARCHAR 32, 用户消息填 `'user'`)、msg_index (INT)、role (VARCHAR 16, 可为 NULL)、event_type (VARCHAR 16)、content (MEDIUMTEXT)、trace_id (CHAR 36)、client_msg_id (CHAR 36, 可为 NULL, 遗留行为 NULL)、update_time (TIMESTAMP)，且 client_msg_id 上存在 UNIQUE 索引

#### Scenario: client_msg_id minted per message

- **WHEN** 一轮 turn 的消息批次持久化
- **THEN** 批内每条消息携带唯一 `client_msg_id`，重复投递时由 UNIQUE 索引 + INSERT IGNORE 保证只落一行

#### Scenario: event_type values

- **WHEN** 系统向 messages 表写入一行
- **THEN** event_type 取值为下列之一：`message`（用户完整消息）、`token`（assistant 内容增量）、`tool_call`（assistant 发起的工具调用）、`tool_result`（工具执行结果）、`agent_start`（agent 开始执行）、`agent_end`（agent 正常结束）、`agent_error`（agent 报错）

#### Scenario: role column nullable for marker events

- **WHEN** 写入 event_type 为 `agent_start`、`agent_end` 的 marker 行
- **THEN** role 列 SHALL 为 NULL

#### Scenario: role for tool result

- **WHEN** 写入 event_type 为 `tool_result` 的行
- **THEN** role 列 SHALL 为 `'tool'`

#### Scenario: msg_index per-turn semantics

- **WHEN** 用户发送一条消息触发一次 turn
- **THEN** 用户消息行的 msg_index SHALL 为 0；同一 turn 内 assistant 事件的 msg_index SHALL 从 1 严格单调递增；下一个 turn 的用户消息 msg_index 重新从 0 开始

#### Scenario: User message agent value

- **WHEN** 写入用户消息行
- **THEN** agent 列 SHALL 填 `'user'`，event_type SHALL 填 `'message'`，role SHALL 填 `'user'`

### Requirement: Session list

系统 SHALL 返回当前用户的会话列表，包含 session_id 和标题。`sessions.update_time` SHALL 由消息 flusher 在批插入成功后对涉及的会话刷新（滞后不超过 `messages.flush_interval`）。

#### Scenario: List sessions

- **WHEN** 用户发送 GET /api/v1/sessions
- **THEN** 系统返回 HTTP 200，body 为会话数组，每项包含 session_id 和 title，按 update_time 降序排列

#### Scenario: Session update_time reflects latest message activity

- **WHEN** 用户向一个已存在的会话发送新消息且该 turn 的消息成功持久化
- **THEN** 消息 flusher 在该批消息插入 MySQL 成功后 SHALL 刷新该会话的 `sessions.update_time`
- **AND THEN** 该会话在后续 GET /api/v1/sessions 列表中出现在最前面（刷新滞后不超过 flush_interval）

#### Scenario: Empty session list

- **WHEN** 用户没有任何会话
- **THEN** 系统返回 HTTP 200，body 为空数组 []
