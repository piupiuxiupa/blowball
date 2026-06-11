## ADDED Requirements

### Requirement: Create session
系统 SHALL 在用户首次发送消息时自动创建会话，生成全局唯一 session_id (UUID)。

#### Scenario: Auto create on first message
- **WHEN** 用户发送 POST /api/v1/sessions/:session_id/messages，session_id 为新 UUID
- **THEN** 系统创建新会话记录，关联 user_id 和 session_id

### Requirement: Send message and stream response
系统 SHALL 接受用户消息并通过 SSE 流式返回 Agent 响应。

#### Scenario: Successful message streaming
- **WHEN** 用户发送 POST /api/v1/sessions/:session_id/messages，body 包含 {"content": "..."}
- **THEN** 系统返回 Content-Type: text/event-stream，逐个推送 StreamEvent

#### Scenario: SSE event format
- **WHEN** 系统推送流式事件
- **THEN** 每个 SSE 事件格式为 "event: <type>\ndata: <json>\n\n"，type 为 agent_start | token | tool_call | agent_end | agent_error | done

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
消息 SHALL 按顺序写入三层存储：Redis 缓存 → 用户目录文件 → MySQL。

#### Scenario: Write message to all layers
- **WHEN** 用户消息或 Agent 响应需要持久化
- **THEN** 系统先写入 Redis（带 TTL），然后异步写入用户 data/{user_uuid}/sessions/{session_id}.json，最后写入 MySQL messages 表

#### Scenario: Redis write failure
- **WHEN** Redis 不可用
- **THEN** 系统跳过 Redis 直接写文件和 MySQL，记录错误日志，不阻塞响应

### Requirement: Message recovery with fallback
系统 SHALL 按优先级恢复会话消息：Redis → 用户目录文件 → MySQL。

#### Scenario: Recover from Redis cache
- **WHEN** 加载会话消息且 Redis 中存在缓存
- **THEN** 系统从 Redis 读取消息列表，不查询文件和数据库

#### Scenario: Fallback to file storage
- **WHEN** Redis 缓存未命中且用户目录存在 session JSON 文件
- **THEN** 系统从文件读取消息，并回填到 Redis 缓存

#### Scenario: Final fallback to MySQL
- **WHEN** Redis 和文件均不可用
- **THEN** 系统从 MySQL messages 表查询，回填 Redis 和文件

### Requirement: Message data model
每条消息 SHALL 包含 session_id、msg_time、agent、msg_index、role (user/assistant/tool)、content、trace_id。

#### Scenario: Message record structure
- **WHEN** 消息写入 MySQL
- **THEN** messages 表包含字段：id (AUTO_INCREMENT)、session_id (VARCHAR 36)、msg_time (TIMESTAMP)、agent (VARCHAR 32)、msg_index (INT)、role (VARCHAR 16)、content (TEXT)、trace_id (VARCHAR 36)、update_time (TIMESTAMP)
