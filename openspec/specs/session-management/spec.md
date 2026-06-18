# session-management Specification

## Purpose

定义会话管理能力，包括按需创建会话、SSE 流式响应、会话列表、自动标题生成、三层消息存储（Redis → 文件 → MySQL）、消息恢复降级策略以及消息数据模型。

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
系统 SHALL 接受用户消息并通过 SSE 流式返回 Agent 响应。发送消息前，系统 SHALL 校验 session 存在且属于当前用户。

#### Scenario: Successful message streaming
- **WHEN** 用户发送 POST /api/v1/sessions/:session_id/messages，body 包含 {"content": "..."}，且 session_id 属于当前用户
- **THEN** 系统返回 Content-Type: text/event-stream，逐个推送 StreamEvent

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
系统 SHALL 返回当前用户的会话列表，包含 session_id 和标题。

#### Scenario: List sessions
- **WHEN** 用户发送 GET /api/v1/sessions
- **THEN** 系统返回 HTTP 200，body 为会话数组，每项包含 session_id 和 title，按 update_time 降序排列

#### Scenario: Empty session list
- **WHEN** 用户没有任何会话
- **THEN** 系统返回 HTTP 200，body 为空数组 []

### Requirement: Auto generate session title
系统 SHALL 在用户首条消息发出后，异步调用 OpenAI 根据用户提问和 Agent 回答生成简短标题。

#### Scenario: Title generated after first exchange
- **WHEN** 用户在新会话中发送首条消息并收到完整回复
- **THEN** 系统异步调用 OpenAI，生成不超过 20 字的简短标题，写入 titles 表

#### Scenario: Title generation failure
- **WHEN** 标题生成调用 OpenAI 失败
- **THEN** 系统使用用户消息前 20 字符作为默认标题，记录警告日志

### Requirement: Three-layer message storage

消息 SHALL 按顺序写入三层存储：Redis 缓存 → 用户目录文件 → MySQL。同一 turn 内，用户消息与 assistant 事件在 orchestrator 成功完成后通过同一个 goroutine 作为一批消息写入三层存储；orchestrator 失败时该 turn 的全部消息均不写入。

#### Scenario: Batch write user and assistant messages after success

- **WHEN** orchestrator 成功完成一次 turn
- **THEN** 系统 SHALL 通过 goroutine 将用户消息与全部 assistant 事件作为一批消息写入三层存储（Redis 批量 RPUSH、FS 一次 read-modify-write 追加、MySQL 一条多 VALUES INSERT）
- **AND THEN** batch 内消息顺序为 user message（msg_index=0）后接 assistant 事件（msg_index 从 1 严格单调递增）

#### Scenario: Skip persistence on orchestrator failure

- **WHEN** orchestrator 返回错误
- **THEN** 系统 SHALL 丢弃本次收集的所有 assistant 事件以及该 turn 的用户消息，不写入任何存储层

#### Scenario: Redis write failure

- **WHEN** Redis 不可用
- **THEN** 系统跳过 Redis 直接写文件和 MySQL，记录错误日志，不阻塞响应

#### Scenario: Goroutine persistence independent of request context

- **WHEN** 客户端在 SSE 流结束前断开连接（取消请求 ctx）
- **THEN** 批量入库 goroutine SHALL 使用派生自 `context.Background()` 的 detached ctx 继续完成写入（保留 trace_id），写入内容包含该 turn 的用户消息与 assistant 事件

#### Scenario: Batch write failure after SSE completes

- **WHEN** 异步批量写入发生 FS 或 MySQL 错误
- **THEN** 系统记录错误日志，该 turn 的用户消息与 assistant 事件均不保证持久化；客户端已收到的 SSE 响应不受影响

### Requirement: Message recovery with fallback
系统 SHALL 按优先级恢复会话消息：Redis → 用户目录文件 → MySQL，并按 `(msg_time, msg_index)` 升序排列。

#### Scenario: Recover from Redis cache
- **WHEN** 加载会话消息且 Redis 中存在缓存
- **THEN** 系统从 Redis 读取消息列表，按 `(msg_time, msg_index)` 升序返回，不查询文件和数据库

#### Scenario: Fallback to file storage
- **WHEN** Redis 缓存未命中且用户目录存在 session JSON 文件
- **THEN** 系统从文件读取消息并回填到 Redis 缓存，返回顺序仍按 `(msg_time, msg_index)` 升序

#### Scenario: Final fallback to MySQL
- **WHEN** Redis 和文件均不可用
- **THEN** 系统从 MySQL messages 表查询（`ORDER BY msg_time ASC, msg_index ASC`），回填 Redis 和文件

#### Scenario: Recovery returns full event stream
- **WHEN** 系统恢复一个已存在的会话历史
- **THEN** 返回的 message 序列 SHALL 包含每个 turn 的用户消息以及 assistant 在该 turn 内产生的所有事件（token / tool_call / agent_start / agent_end / agent_error），按产生顺序排列

### Requirement: Message data model
每条消息 SHALL 包含 session_id、msg_time、agent、msg_index、role、event_type、content、trace_id。`event_type` 标识消息的语义类别，`agent` 标识消息的产生方（用户消息填 `'user'`，assistant 事件填产生该事件的 agent 名）。

#### Scenario: Message record structure
- **WHEN** 消息写入 MySQL
- **THEN** messages 表包含字段：id (BIGINT AUTO_INCREMENT)、session_id (CHAR 36)、msg_time (TIMESTAMP(3) 毫秒精度)、agent (VARCHAR 32, 用户消息填 `'user'`)、msg_index (INT)、role (VARCHAR 16, 可为 NULL)、event_type (VARCHAR 16)、content (MEDIUMTEXT)、trace_id (CHAR 36)、update_time (TIMESTAMP)

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
系统 SHALL 在 orchestrator 执行期间收集其产生的所有 StreamEvent 到内存，作为该 turn 的待入库事件流；该事件流不经过任何拼接或内容合并，保持模型原始输出形态。

#### Scenario: OrchestratorRunner returns raw events
- **WHEN** orchestrator 完成（无论成功或失败）
- **THEN** `OrchestratorRunner.Handle` SHALL 返回 `([]StreamEvent, error)`，切片中每个元素对应 orchestrator 流式输出的一个事件（token / tool_call / agent_start / agent_end / agent_error），顺序与事件实际产生顺序一致

#### Scenario: Sub-agent events included
- **WHEN** Confuse 通过 tool_call 调用 Chongzhi 或 Liang
- **THEN** 子 agent 产生的 token、agent_start、agent_end、agent_error 事件 SHALL 一并进入收集切片，并在 agent 列保留对应的子 agent 名（`Chongzhi` / `Liang`）

#### Scenario: done event excluded from persistence
- **WHEN** orchestrator 发出 `EventDone` 终止事件
- **THEN** 该事件 SHALL 出现在 SSE 流中，但 SHALL NOT 出现在返回给 handler 的事件切片中（usage 元数据不写库）

#### Scenario: tool_call event content shape
- **WHEN** orchestrator 产生 `EventToolCall` 事件
- **THEN** 入库时 content 列 SHALL 存 JSON 序列化的 `{"tool_call_id":"...","name":"<tool_name>","args":<args_json>}` 结构，event_type 为 `tool_call`，role 为 `'assistant'`

#### Scenario: tool_result event content shape
- **WHEN** orchestrator 产生 `EventToolResult` 事件
- **THEN** 入库时 content 列 SHALL 存 JSON 序列化的 `{"tool_call_id":"...","output":<output_json_or_string>}` 结构，event_type 为 `tool_result`，role 为 `'tool'`，agent 为产生该结果的 agent 名
