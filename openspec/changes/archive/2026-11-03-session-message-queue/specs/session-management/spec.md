# session-management Delta

## MODIFIED Requirements

### Requirement: Send message and stream response

系统 SHALL 接受用户消息并通过 SSE 流式返回 Agent 响应。发送消息前，系统 SHALL 校验 session 存在且属于当前用户。该 session 已有运行中 turn 时：`messages.queue.enabled` 为 false（默认）SHALL 返回 409 `SESSION_BUSY` 并携带当前 run id（互斥与恢复语义见 turn-run-lifecycle 规格）；为 true SHALL 接受请求并将消息入队，SSE 流以 `queued` 事件开始，接续起轮后事件在同一流上继续（排队、接续与持久化语义见 session-message-queue 规格）。系统 SHALL 为被接受的请求签发 run id，并通过 `X-Run-Id` 响应头与首个 `agent_start` 事件的 `meta.run_id` 下发（排队消息的 `agent_start` 在其接续起轮后出现）。

#### Scenario: Successful message streaming

- **WHEN** 用户发送 POST /api/v1/sessions/:session_id/messages，body 包含 {"content": "..."}，且 session_id 属于当前用户
- **THEN** 系统返回 Content-Type: text/event-stream，逐个推送 StreamEvent

#### Scenario: Session busy

- **WHEN** 用户发送 POST /api/v1/sessions/:session_id/messages，但该 session 已有运行中 turn，且 `messages.queue.enabled` 为 false（默认）
- **THEN** 系统返回 HTTP 409，body 为 `{"error": {"code": "SESSION_BUSY", "message": ..., "run_id": "<当前运行中 turn 的 run id>"}}`

#### Scenario: Session busy with queueing enabled

- **WHEN** 用户发送 POST /api/v1/sessions/:session_id/messages，但该 session 已有运行中 turn，且 `messages.queue.enabled` 为 true 且队列未满
- **THEN** 系统返回 Content-Type: text/event-stream，携带 `X-Run-Id` 头，流上首事件为 `queued`
- **AND** 该消息在其上一轮终局后被接续为下一轮，`agent_start` 起的事件在同一流上继续

#### Scenario: Run id 下发

- **WHEN** 消息请求被接受并开始流式响应
- **THEN** 响应携带 `X-Run-Id` 头（值为该请求的 trace_id），且首个 `agent_start` 事件的 `meta.run_id` 携带相同值

#### Scenario: Session not found

- **WHEN** 用户发送 POST /api/v1/sessions/:session_id/messages，但 session_id 不存在
- **THEN** 系统返回 HTTP 404，body 为统一错误格式 {"error": {"code": "NOT_FOUND", "message": "session not found"}}

#### Scenario: SSE event format

- **WHEN** 系统推送流式事件
- **THEN** 每个 SSE 事件格式为 "event: <type>\ndata: <json>\n\n"，type 为 agent_start | token | tool_call | tool_result | agent_end | agent_error | done，队列启用时另有 queued

### Requirement: Session list

系统 SHALL 返回当前用户的会话列表，包含 session_id 和标题，并为每个存在运行中 turn 的会话标记 `generating: true`（判据为该会话的活动 run 认领）。`messages.queue.enabled` 为 true 时，列表项 SHALL 为队列深度大于 0 的会话附加携带 `queued_count`（该会话排队条目数；深度为 0 或队列禁用时不携带该字段——additive，旧客户端可忽略）。`sessions.update_time` SHALL 由消息 flusher 在批插入成功后对涉及的会话刷新（滞后不超过 `messages.flush_interval`）。

#### Scenario: List sessions

- **WHEN** 用户发送 GET /api/v1/sessions
- **THEN** 系统返回 HTTP 200，body 为会话数组，每项包含 session_id 和 title，按 update_time 降序排列

#### Scenario: Generating flag

- **WHEN** 用户发送 GET /api/v1/sessions，且某会话存在运行中 turn
- **THEN** 该会话项携带 `"generating": true` 与 `"run_id": "<当前运行中 turn 的 run id>"`（reload 后前端恢复 attach/cancel 目标的权威渠道）
- **AND** 无运行中 turn 的会话 generating 为 false 且不带 run_id

#### Scenario: Queued count flag

- **WHEN** 用户发送 GET /api/v1/sessions，`messages.queue.enabled` 为 true，且某会话队列深度大于 0
- **THEN** 该会话项携带 `"queued_count": <深度>`
- **AND** 队列深度为 0 或队列禁用的会话不携带该字段

#### Scenario: Session update_time reflects latest message activity

- **WHEN** 用户向一个已存在的会话发送新消息且该 turn 的消息成功持久化
- **THEN** 消息 flusher 在该批消息插入 MySQL 成功后 SHALL 刷新该会话的 `sessions.update_time`
- **AND THEN** 该会话在后续 GET /api/v1/sessions 列表中出现在最前面（刷新滞后不超过 flush_interval）

#### Scenario: Empty session list

- **WHEN** 用户没有任何会话
- **THEN** 系统返回 HTTP 200，body 为空数组 []

### Requirement: Session detail

系统 SHALL 提供已鉴权的单会话详情端点 `GET /api/v1/sessions/:session_id`，仅会话所有者可读。响应 SHALL 为会话列表条目的超集：`session_id`、`title`（无标题时空字符串）、`create_time`、`update_time`（RFC3339 UTC），以及与列表一致的活跃 turn 标记 —— 存在运行中 turn 时 `generating: true` 且携带 `run_id`（活动 run 认领判据，同 "Session list" 需求），否则 `generating: false` 且不携带 `run_id`。`messages.queue.enabled` 为 true 且队列深度大于 0 时 SHALL 携带 `queued_count`（与列表一致；禁用或深度为 0 时不携带）。不存在的会话与不属于当前用户的会话 SHALL 返回 404，且不泄露他人会话的存在性。`context_compacted` 等内部标记 SHALL NOT 出现在响应中。

#### Scenario: Retrieve session detail

- **WHEN** 用户发送 GET /api/v1/sessions/:session_id，会话存在且属于当前用户
- **THEN** 系统返回 HTTP 200，body 包含 `session_id`、`title`、`create_time`、`update_time`、`generating` 字段

#### Scenario: Generating flag mirrors active run claim

- **WHEN** 该会话存在运行中 turn
- **THEN** 响应携带 `"generating": true` 与 `"run_id": "<当前运行中 turn 的 run id>"`
- **AND** 无运行中 turn 时 `generating` 为 false 且不携带 `run_id`

#### Scenario: Queued count mirrors queue depth

- **WHEN** `messages.queue.enabled` 为 true 且该会话队列深度大于 0
- **THEN** 响应携带 `"queued_count": <深度>`
- **AND** 队列禁用或深度为 0 时不携带该字段

#### Scenario: Active-run read failure degrades

- **WHEN** 活跃 run 认领读取失败（如 Redis 不可用）
- **THEN** 端点仍返回 HTTP 200，`generating` 为 false，服务端记录 WARN 日志

#### Scenario: Unauthorized session access

- **WHEN** 用户请求不属于自己会话的详情
- **THEN** 系统返回 HTTP 404

#### Scenario: Missing session

- **WHEN** 用户请求不存在的 session_id
- **THEN** 系统返回 HTTP 404

#### Scenario: Title read failure degrades

- **WHEN** 会话行读取成功但标题读取失败
- **THEN** 端点仍返回 HTTP 200，`title` 为空字符串，服务端记录 WARN 日志
