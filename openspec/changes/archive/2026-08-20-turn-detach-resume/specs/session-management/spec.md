# session-management 变更规格

## MODIFIED Requirements

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
