# session-management Specification

## Purpose

定义会话管理能力，包括按需创建会话、SSE 流式响应、会话列表、自动标题生成、Redis-first 双层消息存储（Redis 读缓存 + 入库队列 → MySQL，见 message-write-behind）、消息恢复降级策略以及消息数据模型。

## Requirements

### Requirement: Create session
系统 SHALL 在用户调用 POST /api/v1/sessions 时由服务端生成 session_id（UUID v7）并创建会话。客户端不再负责生成 session_id，首次发消息前必须先创建会话。

#### Scenario: Auto create on first message
- **WHEN** 用户发送 POST /api/v1/sessions/:session_id/messages，session_id 为服务端此前通过 POST /api/v1/sessions 生成的 UUID
- **THEN** 系统复用该会话记录并继续处理消息

#### Scenario: Session must exist before sending message
- **WHEN** 用户发送 POST /api/v1/sessions/:session_id/messages，session_id 不存在或属于其他用户
- **THEN** 系统返回 HTTP 404，不自动创建会话

### Requirement: Send message and stream response
系统 SHALL 接受用户消息并通过 SSE 流式返回 Agent 响应。发送消息前，系统 SHALL 校验 session 存在且属于当前用户。该 session 已有运行中 turn 时，系统 SHALL 返回 409 `SESSION_BUSY` 并携带当前 run id（互斥与恢复语义见 turn-run-lifecycle 规格）。系统 SHALL 为被接受的请求签发 run id，并通过 `X-Run-Id` 响应头与首个 `agent_start` 事件的 `meta.run_id` 下发。

#### Scenario: Successful message streaming
- **WHEN** 用户发送 POST /api/v1/sessions/:session_id/messages，body 包含 {"content": "..."}，且 session_id 属于当前用户
- **THEN** 系统返回 Content-Type: text/event-stream，逐个推送 StreamEvent

#### Scenario: Session busy
- **WHEN** 用户发送 POST /api/v1/sessions/:session_id/messages，但该 session 已有运行中 turn
- **THEN** 系统返回 HTTP 409，body 为 `{"error": {"code": "SESSION_BUSY", "message": ..., "run_id": "<当前运行中 turn 的 run id>"}}`

#### Scenario: Run id 下发
- **WHEN** 消息请求被接受并开始流式响应
- **THEN** 响应携带 `X-Run-Id` 头（值为该请求的 trace_id），且首个 `agent_start` 事件的 `meta.run_id` 携带相同值

#### Scenario: Session not found
- **WHEN** 用户发送 POST /api/v1/sessions/:session_id/messages，但 session_id 不存在
- **THEN** 系统返回 HTTP 404，body 为统一错误格式 {"error": {"code": "NOT_FOUND", "message": "session not found"}}

#### Scenario: SSE event format
- **WHEN** 系统推送流式事件
- **THEN** 每个 SSE 事件格式为 "event: <type>\ndata: <json>\n\n"，type 为 agent_start | token | tool_call | tool_result | agent_end | agent_error | done

### Requirement: Server-generated session ID
服务端生成 session_id 时 SHALL 使用 UUID v7，确保 ID 按时间有序且与 users.user_id、trace_id 的生成策略保持一致。

#### Scenario: Session ID is UUID v7
- **WHEN** 系统创建新会话
- **THEN** session_id 是一个符合 UUID v7 规范的 36 字符字符串

### Requirement: Session list
系统 SHALL 返回当前用户的会话列表，包含 session_id 和标题，并为每个存在运行中 turn 的会话标记 `generating: true`（判据为该会话的活动 run 认领）。`sessions.update_time` SHALL 由消息 flusher 在批插入成功后对涉及的会话刷新（滞后不超过 `messages.flush_interval`）。

#### Scenario: List sessions
- **WHEN** 用户发送 GET /api/v1/sessions
- **THEN** 系统返回 HTTP 200，body 为会话数组，每项包含 session_id 和 title，按 update_time 降序排列

#### Scenario: Generating flag
- **WHEN** 用户发送 GET /api/v1/sessions，且某会话存在运行中 turn
- **THEN** 该会话项携带 `"generating": true` 与 `"run_id": "<当前运行中 turn 的 run id>"`（reload 后前端恢复 attach/cancel 目标的权威渠道）
- **AND** 无运行中 turn 的会话 generating 为 false 且不带 run_id

#### Scenario: Session update_time reflects latest message activity
- **WHEN** 用户向一个已存在的会话发送新消息且该 turn 的消息成功持久化
- **THEN** 消息 flusher 在该批消息插入 MySQL 成功后 SHALL 刷新该会话的 `sessions.update_time`
- **AND THEN** 该会话在后续 GET /api/v1/sessions 列表中出现在最前面（刷新滞后不超过 flush_interval）

#### Scenario: Empty session list
- **WHEN** 用户没有任何会话
- **THEN** 系统返回 HTTP 200，body 为空数组 []

### Requirement: Auto generate session title
系统 SHALL 在用户首条消息发出后，异步调用 OpenAI 根据用户提问和 Agent 回答生成简短标题，但 SHALL NOT 覆盖用户手动设置的标题。

#### Scenario: Title generated after first exchange
- **WHEN** 用户在新会话中发送首条消息并收到完整回复
- **THEN** 系统异步调用 OpenAI，生成不超过 20 字的简短标题，写入 titles 表，且 `is_manual = FALSE`

#### Scenario: Title generation failure
- **WHEN** 标题生成调用 OpenAI 失败
- **THEN** 系统使用用户消息前 20 字符作为默认标题，记录警告日志

#### Scenario: Manual title is not overwritten
- **WHEN** 用户此前已通过 `PATCH /api/v1/sessions/:session_id` 设置过该会话标题
- **THEN** 系统自动标题生成跳过 LLM 调用，且不修改 `titles` 行

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

### Requirement: Assistant event stream collection
系统 SHALL 在 orchestrator 执行期间收集其产生的所有 StreamEvent 到内存，作为该 turn 的待入库事件流；该事件流不经过任何拼接或内容合并，保持模型原始输出形态。收集侧 SHALL 同步将每个事件（含 `done`）追加到该 run 的 Redis 事件日志（追加失败仅告警，详见 turn-run-lifecycle 规格）。

#### Scenario: OrchestratorRunner returns raw events
- **WHEN** orchestrator 完成（无论成功或失败）
- **THEN** `OrchestratorRunner.Handle` SHALL 返回 `([]StreamEvent, error)`，切片中每个元素对应 orchestrator 流式输出的一个事件（token / tool_call / agent_start / agent_end / agent_error），顺序与事件实际产生顺序一致

#### Scenario: Sub-agent events included
- **WHEN** Confucius 通过 tool_call 调用 Chongzhi 或 Liang
- **THEN** 子 agent 产生的 token、agent_start、agent_end、agent_error 事件 SHALL 一并进入收集切片，并在 agent 列保留对应的子 agent 名（`Chongzhi` / `Liang`）

#### Scenario: Events feed the run event log
- **WHEN** orchestrator 产生任意事件（含终局 `done` 事件）
- **THEN** 收集侧 SHALL 将该事件追加到 `run:{rid}:events`；`done` 事件因此可被恢复端点回放

#### Scenario: done event excluded from persistence
- **WHEN** orchestrator 发出 `EventDone` 终止事件
- **THEN** 该事件 SHALL 出现在 SSE 流中，但 SHALL NOT 出现在返回给 handler 的事件切片中（usage 元数据不写库）

#### Scenario: tool_call event content shape
- **WHEN** orchestrator 产生 `EventToolCall` 事件
- **THEN** 入库时 content 列 SHALL 存 JSON 序列化的 `{"tool_call_id":"...","name":"<tool_name>","args":<args_json>}` 结构，event_type 为 `tool_call`，role 为 `'assistant'`

#### Scenario: tool_result event content shape
- **WHEN** orchestrator 产生 `EventToolResult` 事件
- **THEN** 入库时 content 列 SHALL 存 JSON 序列化的 `{"tool_call_id":"...","output":<output_json_or_string>}` 结构，event_type 为 `tool_result`，role 为 `'tool'`，agent 为产生该结果的 agent 名
