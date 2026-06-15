## MODIFIED Requirements

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

## ADDED Requirements

### Requirement: Server-generated session ID
服务端生成 session_id 时 SHALL 使用 UUID v7，确保 ID 按时间有序且与 users.user_id、trace_id 的生成策略保持一致。

#### Scenario: Session ID is UUID v7
- **WHEN** 系统创建新会话
- **THEN** session_id 是一个符合 UUID v7 规范的 36 字符字符串
