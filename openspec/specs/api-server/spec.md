# api-server Specification

## Purpose

定义后端 HTTP API 服务能力，包括 Gin HTTP 服务、CORS 中间件、统一错误响应、API 路由注册、trace_id 请求上下文、结构化日志以及 graceful shutdown。

## Requirements

### Requirement: Gin HTTP server
系统 SHALL 使用 Gin 框架启动 HTTP 服务，监听配置文件中指定的端口。

#### Scenario: Server starts on configured port
- **WHEN** 服务启动，config.yaml 中 server.port 为 8080
- **THEN** HTTP 服务监听 0.0.0.0:8080

### Requirement: CORS middleware
系统 SHALL 配置 CORS 中间件，允许前端跨域访问。

#### Scenario: CORS headers in response
- **WHEN** 前端发送 OPTIONS 预检请求
- **THEN** 响应包含 Access-Control-Allow-Origin、Access-Control-Allow-Methods、Access-Control-Allow-Headers

### Requirement: Unified error response
系统 SHALL 使用统一的错误响应格式。

#### Scenario: Error response format
- **WHEN** 任何接口返回错误
- **THEN** 响应 body 为 {"error": {"code": "ERROR_CODE", "message": "描述信息"}}

### Requirement: API routing
系统 SHALL 注册以下 API 路由组。

#### Scenario: Auth routes
- **WHEN** 服务启动
- **THEN** 注册 POST /api/v1/auth/login（无需鉴权）

#### Scenario: Session routes
- **WHEN** 服务启动
- **THEN** 注册以下需要鉴权的路由：
  - GET /api/v1/sessions
  - POST /api/v1/sessions/:session_id/messages

#### Scenario: Workspace routes
- **WHEN** 服务启动
- **THEN** 注册以下需要鉴权的路由：
  - GET /api/v1/workspace/files
  - POST /api/v1/workspace/upload
  - GET /api/v1/workspace/files/*path
  - GET /api/v1/workspace/files/*path/content

#### Scenario: Tool and skill routes
- **WHEN** 服务启动
- **THEN** 注册以下需要鉴权的路由：
  - GET /api/v1/mcp/tools
  - GET /api/v1/skills

### Requirement: Request context with trace_id
系统 SHALL 为每个 HTTP 请求生成唯一 trace_id，贯穿整个请求链路。

#### Scenario: Trace ID generated per request
- **WHEN** 任何 API 请求到达
- **THEN** 中间件生成 UUID 格式的 trace_id，写入 gin.Context，传递到 service、agent、store 各层

#### Scenario: Trace ID in logs
- **WHEN** 请求处理过程中记录日志
- **THEN** 日志包含 trace_id 字段，可按 trace_id 追踪完整请求链路

### Requirement: Structured logging
系统 SHALL 使用 zap 结构化日志，关键操作必须记录日志。

#### Scenario: Log format
- **WHEN** 服务运行
- **THEN** 日志使用 JSON 格式，包含 timestamp、level、trace_id、message 等字段

#### Scenario: Key operation logging
- **WHEN** 以下操作发生时
- **THEN** 记录日志：用户登录、Agent 调用开始/结束、tool 调用、消息存储写入、错误发生

### Requirement: Graceful shutdown
系统 SHALL 支持 graceful shutdown，收到 SIGTERM/SIGINT 时优雅退出。

#### Scenario: Graceful shutdown on signal
- **WHEN** 服务收到 SIGTERM 或 SIGINT 信号
- **THEN** 系统停止接收新请求，等待进行中的请求完成（最长 10 秒），然后关闭数据库和 Redis 连接
