## MODIFIED Requirements

### Requirement: API routing
系统 SHALL 注册以下 API 路由组。

#### Scenario: Session routes
- **WHEN** 服务启动
- **THEN** 注册以下需要鉴权的路由：
  - GET /api/v1/sessions
  - POST /api/v1/sessions
  - GET /api/v1/sessions/:session_id/messages
  - POST /api/v1/sessions/:session_id/messages

### Requirement: Request context with trace_id
系统 SHALL 为每个 HTTP 请求生成唯一 trace_id，贯穿整个请求链路。

#### Scenario: Trace ID generated per request
- **WHEN** 任何 API 请求到达
- **THEN** 中间件生成 UUID v7 格式的 trace_id，写入 gin.Context，传递到 service、agent、store 各层

#### Scenario: Trace ID in logs
- **WHEN** 请求处理过程中记录日志
- **THEN** 日志包含 trace_id 字段，可按 trace_id 追踪完整请求链路

## ADDED Requirements

### Requirement: Session creation route
系统 SHALL 暴露 POST /api/v1/sessions 路由，用于服务端生成并返回新的 session_id。

#### Scenario: Route is authenticated
- **WHEN** 服务启动
- **THEN** POST /api/v1/sessions 位于鉴权路由组内，未携带有效 token 时返回 401

### Requirement: Session messages route
系统 SHALL 暴露 GET /api/v1/sessions/:session_id/messages 路由，用于分页读取会话历史消息。

#### Scenario: Route is authenticated
- **WHEN** 服务启动
- **THEN** GET /api/v1/sessions/:session_id/messages 位于鉴权路由组内，未携带有效 token 时返回 401
