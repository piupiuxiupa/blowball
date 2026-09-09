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

### Requirement: Message input limit

系统 SHALL 对 `POST /api/v1/sessions/:session_id/messages` 的用户输入设置两层入口防线，且全部校验 SHALL 在任何存储读写与该会话的 run 认领（见 turn-run-lifecycle）之前完成：

1. **请求体字节上限**：JSON 解析前请求体 SHALL 被限制在固定字节上限（1MB 常量，非配置项）内；超限请求 SHALL 返回 `413` 错误码 `REQUEST_TOO_LARGE`。
2. **输入 token 上限**：`content` 的估算 token 数超过 `messages.max_input_tokens` 时，请求 SHALL 被拒绝并返回 `400` 错误码 `CONTENT_TOO_LONG`，message SHALL 携带生效上限值与估算值。

token 数 SHALL 采用保守的字符类启发式估算（CJK 类 rune 每 1 个计 1 token，其余每 4 个字符计 1 token），估算结果 SHALL NOT 依赖网关侧精确计数。配置语义：`messages.max_input_tokens` 未配置时生效上限 SHALL 为 5000；显式配置 `0` SHALL 关闭 token 检测（字节上限不受影响）；负值 SHALL 在配置加载时被拒绝。校验拒绝的请求 SHALL NOT 认领该会话的 run 槽，也 SHALL NOT 触发任何 LLM 调用。

#### Scenario: Oversized request body

- **WHEN** 用户发送 POST /messages，请求体超过字节上限
- **THEN** 系统返回 HTTP 413，body 为 `{"error": {"code": "REQUEST_TOO_LARGE", ...}}`
- **AND** 该请求未读取任何存储、未认领该会话的 run 槽

#### Scenario: Content exceeds token limit

- **WHEN** 用户发送 POST /messages，`content` 的估算 token 数超过生效上限（默认 5000）
- **THEN** 系统返回 HTTP 400，body 为 `{"error": {"code": "CONTENT_TOO_LONG", "message": ...}}`，message 携带上限值与估算 token 数
- **AND** 该请求未认领该会话的 run 槽、未触发 LLM 调用

#### Scenario: Content within token limit passes

- **WHEN** `content` 估算 token 数不超过生效上限
- **THEN** 请求按既有流程继续（校验不改变正常输入的行为）

#### Scenario: Estimation is CJK-aware

- **WHEN** `content` 为纯 CJK 文本，长度等于生效上限字符数
- **THEN** 估算 token 数不低于该字符数（每个 CJK rune 至少计 1 token，估算方向保守偏高）

#### Scenario: Default limit applies when unset

- **WHEN** 配置未设置 `messages.max_input_tokens`
- **THEN** 生效上限为 5000（token 检测默认开启）

#### Scenario: Explicit zero disables token detection

- **WHEN** 配置显式设置 `messages.max_input_tokens: 0`
- **THEN** token 检测关闭（任意长度 `content` 不因 token 上限被拒），字节上限仍然生效

#### Scenario: Negative value rejected at load

- **WHEN** 配置设置 `messages.max_input_tokens` 为负数
- **THEN** 配置加载失败，服务拒绝启动

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

### Requirement: Session detail

系统 SHALL 提供已鉴权的单会话详情端点 `GET /api/v1/sessions/:session_id`，仅会话所有者可读。响应 SHALL 为会话列表条目的超集：`session_id`、`title`（无标题时空字符串）、`create_time`、`update_time`（RFC3339 UTC），以及与列表一致的活跃 turn 标记 —— 存在运行中 turn 时 `generating: true` 且携带 `run_id`（活动 run 认领判据，同 "Session list" 需求），否则 `generating: false` 且不携带 `run_id`。不存在的会话与不属于当前用户的会话 SHALL 返回 404，且不泄露他人会话的存在性。`context_compacted` 等内部标记 SHALL NOT 出现在响应中。

#### Scenario: Retrieve session detail

- **WHEN** 用户发送 GET /api/v1/sessions/:session_id，会话存在且属于当前用户
- **THEN** 系统返回 HTTP 200，body 包含 `session_id`、`title`、`create_time`、`update_time`、`generating` 字段

#### Scenario: Generating flag mirrors active run claim

- **WHEN** 该会话存在运行中 turn
- **THEN** 响应携带 `"generating": true` 与 `"run_id": "<当前运行中 turn 的 run id>"`
- **AND** 无运行中 turn 时 `generating` 为 false 且不携带 `run_id`

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

### Requirement: Auto generate session title
系统 SHALL 在 `POST /messages` 的 session-run claim 成功之后、orchestrator 启动之前,以 fire-and-forget goroutine 异步触发标题生成(与 turn 并行;被 409 `SESSION_BUSY` 拒绝的请求 SHALL NOT 触发)。触发 SHALL 按会话内用户消息序号 n(已持久化 user 行数 + 1,1 基)节流:n % 3 == 1(即第 1、4、7…条用户消息)时触发,其余消息 SHALL NOT 触发。生成输入 SHALL 为该会话首条用户消息与本次触发消息(不包含 assistant 内容)。标题写入 SHALL NOT 覆盖手动标题:`UpsertTitle` SHALL 以 SQL 原子守卫实现——已存在 `is_manual = TRUE` 的行 SHALL 保持原 `title`/`trace_id`/`is_manual` 不变(AI upsert 对其零效果);`is_manual = FALSE` 或不存在的行照常插入/覆盖。生成失败 SHALL 降级为本次触发消息前 20 字符并记警告日志。turn 结束后的持久化路径 SHALL NOT 再触发标题生成。

#### Scenario: 首条消息发送即触发
- **WHEN** 用户在会话中发送第 1 条消息(n=1),turn 尚未开始
- **THEN** 系统在 claim 成功后异步发起标题生成(不等待 turn 完成),生成不超过 20 字的标题写入 titles 表,`is_manual = FALSE`

#### Scenario: 每 3 条消息刷新一次
- **WHEN** 用户依次发送第 2、3、4、5 条消息
- **THEN** 仅第 4 条(n=4,n%3==1)触发标题生成;第 2、3、5 条不触发

#### Scenario: 生成输入为首条 + 本次消息
- **WHEN** 第 4 条消息触发标题生成
- **THEN** LLM 输入包含该会话首条用户消息与第 4 条消息,不包含任何 assistant 内容

#### Scenario: 生成失败降级
- **WHEN** 标题生成调用 OpenAI 失败
- **THEN** 系统使用本次触发消息前 20 字符作为默认标题,记录警告日志

#### Scenario: 会话忙时不触发
- **WHEN** 会话存在运行中 turn,新消息请求被 409 `SESSION_BUSY` 拒绝
- **THEN** 该请求不触发标题生成

#### Scenario: 触发时已是手动标题则早退
- **WHEN** 触发时 `GetTitle` 读到 `is_manual = TRUE` 的标题行
- **THEN** 系统跳过 LLM 调用,不发起生成

#### Scenario: 生成在 flight 中被手动改题不覆盖
- **WHEN** 标题生成已通过 manual 早退检查、LLM 调用进行中,用户此时通过 `PATCH /api/v1/sessions/:session_id` 设置了手动标题
- **THEN** 随后的 AI `UpsertTitle` 对该行零效果:手动标题、`trace_id` 与 `is_manual = TRUE` 全部保持

#### Scenario: 非手动行照常覆盖
- **WHEN** 已存在 `is_manual = FALSE` 的 AI 标题行,新一次生成完成
- **THEN** `UpsertTitle` 覆盖 `title`/`trace_id`,`is_manual` 保持 FALSE

#### Scenario: 中断或失败的 turn 仍有标题
- **WHEN** 触发消息对应的 turn 随后被取消或以错误结束
- **THEN** 标题生成不受影响(发送时已触发、detached context 下完成),turn 的中断路径不再额外触发标题

### Requirement: Redis-first message storage

消息 SHALL 按顺序写入 Redis 的两类 key：`msgs:{session_id}`（读热层，24h TTL）与 `msgs:buffer`（入库队列，无 TTL），二者在同一 pipeline 中批量 RPUSH。**用户消息 SHALL 在 session-run claim 成功后、orchestrator 启动前，作为独立批次先行双写**（`client_msg_id = {trace_id}:0`，msg_index=0，msg_time=请求到达时刻）；**assistant 事件在 orchestrator 结束后作为后缀批次双写**（含 mid-turn 上下文压缩触发的中间批次，见 context-compaction 能力），后续批次 SHALL NOT 再次包含已先行持久化的用户消息行——`msgs:{session_id}` list 无去重语义，该 turn 的用户消息行 SHALL 恰好 append 一次。先行写入返回错误时，turn 结束批次 SHALL 照旧包含用户消息行（确定性幂等键使 MySQL 不产生重复行）。MySQL 写入由后台 flusher 异步批量完成（见 message-write-behind 能力）；系统 SHALL NOT 再写任何文件系统会话文件。orchestrator 失败（含客户端中断）时，已产生的部分事件 SHALL 照常持久化（用户消息此时已先行持久化；与 interrupted-turn-persistence 能力一致）。

#### Scenario: User message persisted at send time

- **WHEN** 用户发送消息且 session-run claim 成功，turn 开始执行
- **THEN** 系统 SHALL 在 orchestrator 启动前将用户消息行（含确定性 `client_msg_id = {trace_id}:0`，msg_index=0）通过同一 Redis 双写路径写入（`RPUSH msgs:{session_id}` 刷新 TTL + `RPUSH msgs:buffer`），元素为同一份消息 JSON
- **AND** 该行在 turn 运行全程对消息恢复可见；MySQL 侧于 ≤ `messages.flush_interval` 窗口内可读

#### Scenario: Turn-end batch is an assistant-event suffix

- **WHEN** orchestrator 完成（成功、失败或取消），turn 结束批次持久化
- **THEN** 批次 SHALL 仅含 assistant 事件（msg_index 从已持久化位置严格单调续接），SHALL NOT 再次包含用户消息行
- **AND** `msgs:{session_id}` 中该 turn 的用户消息行恰好出现一次

#### Scenario: Send-time write failure falls back to turn-end batch

- **WHEN** 发送时的用户消息批次写入返回错误
- **THEN** turn 结束批次 SHALL 照旧包含用户消息行
- **AND** 确定性幂等键（`uk_messages_client_msg_id`）使 MySQL 侧不产生重复行

#### Scenario: Rejected requests write nothing

- **WHEN** 请求因校验失败（400）、会话不存在（404）或已有运行中 turn（409 `SESSION_BUSY`）被拒绝
- **THEN** 系统 SHALL NOT 为该请求写入任何消息行

#### Scenario: Persist partial turn on orchestrator failure

- **WHEN** orchestrator 返回错误（含上游 429/5xx、超时、取消）
- **THEN** 系统 SHALL 照常持久化已收集的部分 assistant 事件（同一 Redis 双写路径），使重载的会话历史与用户实际看到的内容一致
- **AND** 该 turn 的用户消息行已在发送时持久化，不受影响

#### Scenario: Mid-turn visibility after refresh

- **WHEN** turn 运行中用户刷新页面并重进该会话，前端读取历史并 attach 事件流
- **THEN** 历史读取 SHALL 包含该 turn 的用户消息行（发送时已持久化）
- **AND** 事件重放照旧只含 agent 事件（用户消息行不进事件日志），二者共同构成完整视图

#### Scenario: Redis write failure

- **WHEN** Redis 双写 pipeline 失败（Redis 不可用）
- **THEN** 系统 SHALL 降级为同步直写 MySQL（幂等 INSERT），记录错误日志，不丢弃该批消息，不阻塞响应

#### Scenario: Goroutine persistence independent of request context

- **WHEN** 客户端在 SSE 流结束前断开连接（取消请求 ctx）
- **THEN** 批量入库 goroutine SHALL 使用派生自 `context.Background()` 的 detached ctx 继续完成写入（保留 trace_id），写入内容包含该 turn 的 assistant 事件

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

### Requirement: Sub-agent placeholder history mode
系统 SHALL 支持已鉴权的 `GET /api/v1/sessions/:session_id/messages?subagent_content=placeholder` 查询模式。该模式 SHALL 保留非子 Agent 消息与主 Agent 输出；对携带动态子 Agent 身份的行，仅返回 `agent_start`、`agent_end`、`agent_error` lifecycle 占位，MUST NOT 返回其 `token`、`reasoning`、`tool_call` 或 `tool_result` 长内容。与 `subagent_runs.run_id` 对应的父 `spawn_subagent` tool result SHALL 被省略，避免重复返回子 Agent 最终输出。占位过滤 SHALL 在分页前应用，且返回的 lifecycle 行 MUST 继续携带 `agent_instance_id` 与 `run_id` 供详情 API 懒加载。

#### Scenario: Placeholder mode returns lifecycle markers only
- **WHEN** 历史中某个动态子 Agent 包含 agent_start、reasoning、token、tool_call、tool_result 与 agent_end 行
- **THEN** placeholder 模式返回该子 Agent 的 lifecycle marker，并携带相同 `agent_instance_id` / `run_id`
- **AND THEN** 不返回该子 Agent 的 reasoning、token、tool_call 或 tool_result 内容行

#### Scenario: Parent spawn result deduplicated
- **WHEN** Confucius 的 `spawn_subagent` tool result 与某个 `subagent_runs.run_id` 对应
- **THEN** placeholder 模式省略该父 tool result；前端可通过对应子 Agent run detail API 懒加载完整结果

#### Scenario: Full mode remains default
- **WHEN** 请求省略 `subagent_content` 或显式使用 `full`
- **THEN** 响应保持既有完整事件流行为

#### Scenario: Unknown mode rejected
- **WHEN** `subagent_content` 不是 `full` 或 `placeholder`
- **THEN** 系统返回 HTTP 400，且不读取消息分页数据

