## MODIFIED Requirements

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

### Requirement: Assistant event stream collection
系统 SHALL 在 orchestrator 执行期间收集其产生的所有 StreamEvent 到内存，作为该 turn 的待入库事件流；该事件流不经过任何拼接或内容合并，保持模型原始输出形态。

#### Scenario: OrchestratorRunner returns raw events
- **WHEN** orchestrator 完成（无论成功或失败）
- **THEN** `OrchestratorRunner.Handle` SHALL 返回 `([]StreamEvent, error)`，切片中每个元素对应 orchestrator 流式输出的一个事件（token / tool_call / tool_result / agent_start / agent_end / agent_error），顺序与事件实际产生顺序一致

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
- **THEN** 返回的 message 序列 SHALL 包含每个 turn 的用户消息以及 assistant 在该 turn 内产生的所有事件（token / tool_call / tool_result / agent_start / agent_end / agent_error），按产生顺序排列
