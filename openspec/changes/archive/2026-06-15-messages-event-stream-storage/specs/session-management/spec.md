## MODIFIED Requirements

### Requirement: Message data model
每条消息 SHALL 包含 session_id、msg_time、agent、msg_index、role、event_type、content、trace_id。`event_type` 标识消息的语义类别，`agent` 标识消息的产生方（用户消息填 `'user'`，assistant 事件填产生该事件的 agent 名）。

#### Scenario: Message record structure
- **WHEN** 消息写入 MySQL
- **THEN** messages 表包含字段：id (BIGINT AUTO_INCREMENT)、session_id (CHAR 36)、msg_time (TIMESTAMP(3) 毫秒精度)、agent (VARCHAR 32, 用户消息填 `'user'`)、msg_index (INT)、role (VARCHAR 16, 可为 NULL)、event_type (VARCHAR 16)、content (MEDIUMTEXT)、trace_id (CHAR 36)、update_time (TIMESTAMP)

#### Scenario: event_type values
- **WHEN** 系统向 messages 表写入一行
- **THEN** event_type 取值为下列之一：`message`（用户完整消息）、`token`（assistant 内容增量）、`tool_call`（assistant 发起的工具调用）、`agent_start`（agent 开始执行）、`agent_end`（agent 正常结束）、`agent_error`（agent 报错）

#### Scenario: role column nullable for marker events
- **WHEN** 写入 event_type 为 `agent_start`、`agent_end` 的 marker 行
- **THEN** role 列 SHALL 为 NULL

#### Scenario: msg_index per-turn semantics
- **WHEN** 用户发送一条消息触发一次 turn
- **THEN** 用户消息行的 msg_index SHALL 为 0；同一 turn 内 assistant 事件的 msg_index SHALL 从 1 严格单调递增；下一个 turn 的用户消息 msg_index 重新从 0 开始

#### Scenario: User message agent value
- **WHEN** 写入用户消息行
- **THEN** agent 列 SHALL 填 `'user'`，event_type SHALL 填 `'message'`，role SHALL 填 `'user'`

### Requirement: Three-layer message storage
消息 SHALL 按顺序写入三层存储：Redis 缓存 → 用户目录文件 → MySQL。用户消息走单条同步写入；assistant 一轮产生的所有事件仅在 orchestrator 成功完成后通过 goroutine 批量写入三层存储。

#### Scenario: Write user message to all layers synchronously
- **WHEN** 用户消息需要持久化
- **THEN** 系统在 orchestrator 启动前同步写入 Redis（带 TTL）、用户 data/{user_uuid}/sessions/{session_id}.json、MySQL messages 表

#### Scenario: Batch write assistant events after success
- **WHEN** orchestrator 成功完成一次 turn 并返回所有 StreamEvent
- **THEN** 系统 SHALL 通过 goroutine 将全部事件作为一批消息写入三层存储（Redis 批量 RPUSH、FS 一次 read-modify-write 追加、MySQL 一条多 VALUES INSERT）

#### Scenario: Skip persistence on orchestrator failure
- **WHEN** orchestrator 返回错误
- **THEN** 系统 SHALL 丢弃本次收集的所有 assistant 事件，不写入任何存储层；用户消息（已 sync 写入）不受影响

#### Scenario: Redis write failure
- **WHEN** Redis 不可用
- **THEN** 系统跳过 Redis 直接写文件和 MySQL，记录错误日志，不阻塞响应

#### Scenario: Goroutine persistence independent of request context
- **WHEN** 客户端在 SSE 流结束前断开连接（取消请求 ctx）
- **THEN** assistant 事件的批量入库 goroutine SHALL 使用派生自 `context.Background()` 的 detached ctx 继续完成写入（保留 trace_id）

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

## ADDED Requirements

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
- **THEN** 入库时 content 列 SHALL 存 JSON 序列化的 `{"name":"<tool_name>","args":<args_json>}` 结构，event_type 为 `tool_call`，role 为 `'assistant'`
