## 1. Project Setup

- [x] 1.1 Initialize Go module (`go mod init github.com/lush/blowball`) and create project directory structure
- [x] 1.2 Create `config.yaml` with all configuration sections (server, openai, mysql, redis, jwt, agents, tools, logging)
- [x] 1.3 Implement `internal/config/config.go` — load and parse config.yaml, support environment variable substitution
- [x] 1.4 Implement `internal/pkg/logger/zap.go` — initialize structured JSON logger with configurable level
- [x] 1.5 Implement `internal/pkg/trace/trace.go` — UUID-based trace_id generation
- [x] 1.6 Implement `internal/pkg/jwt/jwt.go` — JWT sign and verify with HS256

## 2. Data Model & Database

- [x] 2.1 Create MySQL migration files: `001_users.sql` (user_id PK UUID, username, password, status, trace_id, update_time)
- [x] 2.2 Create MySQL migration files: `002_sessions.sql` (user_id, session_id PK UUID, trace_id, update_time)
- [x] 2.3 Create MySQL migration files: `003_titles.sql` (session_id PK, title, trace_id, update_time)
- [x] 2.4 Create MySQL migration files: `004_messages.sql` (id AUTO_INCREMENT, session_id, msg_time, agent, msg_index, role, content, trace_id, update_time)
- [x] 2.5 Define Go model structs in `internal/model/` for User, Session, Title, Message
- [x] 2.6 Implement `internal/store/mysql/` — sqlx-based CRUD for users, sessions, titles, messages with trace_id logging

## 3. Redis Cache Layer

- [x] 3.1 Implement `internal/store/redis/session.go` — session message cache with configurable TTL (GET/SET/DEL by session_id)
- [x] 3.2 Implement `internal/store/redis/message.go` — message list cache operations (append message, get all messages)
- [x] 3.3 Write unit tests for Redis cache layer (using miniredis or mock)

## 4. File System Store

- [x] 4.1 Implement `internal/store/fs/session.go` — read/write session JSON files to `data/{user_uuid}/sessions/`
- [x] 4.2 Implement user directory auto-creation (sessions/, workspace/, skills/) on first access

## 5. Stream Infrastructure

- [x] 5.1 Define `internal/stream/event.go` — StreamEvent struct (Type, Agent, Content, Meta) and event type constants
- [x] 5.2 Implement `internal/stream/hub.go` — buffered channel management, concurrent write safety, context cancellation support
- [x] 5.3 Implement `internal/stream/sse.go` — SSE writer that consumes StreamEvent channel and writes to gin.ResponseWriter with proper SSE format (event: type\ndata: json\n\n)
- [x] 5.4 Write unit tests for SSE writer (verify event format, context cancellation)

## 6. Auth Module

- [x] 6.1 Implement `internal/middleware/auth.go` — JWT verification middleware, extract user_id from token, inject into gin.Context
- [x] 6.2 Implement `internal/middleware/cors.go` — CORS configuration
- [x] 6.3 Implement `internal/service/auth.go` — login logic (verify password with bcrypt, sign JWT)
- [x] 6.4 Implement `internal/handler/auth.go` — POST /api/v1/auth/login handler
- [x] 6.5 Write unit tests for auth service (login success, invalid credentials, token generation)

## 7. Agent Orchestration Engine

- [x] 7.1 Define `internal/agent/agent.go` — Agent interface (Name, SystemPrompt, Tools, Run method)
- [x] 7.2 Implement `internal/tool/registry.go` — tool registration and lookup by name, build OpenAI tools parameter for agent
- [x] 7.3 Implement Xizhi tool definitions in `internal/tool/xizhi/` — register tool schemas (read_file, write_file, modify_file) from config
- [x] 7.4 Implement `internal/agent/confuse.go` — Confuse agent: Agent Loop with OpenAI streaming, function-calling dispatch, parallel tool execution via errgroup
- [x] 7.5 Implement `internal/agent/chongzhi.go` — Chongzhi agent: isolated context, OpenAI streaming call with Xizhi tools, StreamEvent passthrough
- [x] 7.6 Implement `internal/agent/liang.go` — Liang agent: isolated context, OpenAI streaming call (no tools initially), StreamEvent passthrough
- [x] 7.7 Implement `internal/agent/orchestrator.go` — top-level orchestrator that receives user message, invokes Confuse agent loop, manages StreamEvent channel lifecycle
- [x] 7.8 Write unit tests for agent loop (mock OpenAI responses, verify tool call dispatch, verify parallel execution, verify streaming events)

## 8. Xizhi Tool Implementation

- [x] 8.1 Implement `internal/tool/xizhi/write.go` — write file to user workspace with auto mkdir, path validation
- [x] 8.2 Implement `internal/tool/xizhi/read.go` — read file from user workspace with path validation
- [x] 8.3 Implement `internal/tool/xizhi/modify.go` — replace content in file (old_content → new_content), handle ambiguous matches
- [x] 8.4 Implement `internal/tool/xizhi/landlock.go` — go-landlock initialization on startup, restrict process to data/ directory
- [x] 8.5 Implement path security validation — resolve symlinks, verify prefix, block path traversal
- [x] 8.6 Write unit tests for Xizhi tools (write, read, modify, path validation, security checks)

## 9. Session & Message Service

- [x] 9.1 Implement `internal/service/session.go` — session creation, session list with title, three-layer message save (Redis → FS → MySQL)
- [x] 9.2 Implement `internal/service/message.go` — message recovery with fallback (Redis → FS → MySQL), message append
- [x] 9.3 Implement `internal/service/title.go` — async title generation via OpenAI, fallback to first 20 chars on failure
- [x] 9.4 Write unit tests for message save/recover logic (mock all three store layers)

## 10. HTTP Handlers

- [x] 10.1 Implement `internal/handler/session.go` — POST /api/v1/sessions/:id/messages (SSE streaming), GET /api/v1/sessions (list)
- [x] 10.2 Implement `internal/handler/workspace.go` — GET /api/v1/workspace/files (list), POST /api/v1/workspace/upload, GET /api/v1/workspace/files/*path (download), GET /api/v1/workspace/files/*path/content (text content)
- [x] 10.3 Implement `internal/handler/mcp.go` — GET /api/v1/mcp/tools (return registered tool definitions)
- [x] 10.4 Implement `internal/handler/skill.go` — GET /api/v1/skills (scan user skills directory)

## 11. Server Bootstrap

- [x] 11.1 Implement `cmd/server/main.go` — load config, init logger, init MySQL/Redis connections, apply landlock, register routes, start Gin server with graceful shutdown
- [x] 11.2 Create `Makefile` with targets: build, run, test, migrate, lint
- [x] 11.3 Create `docker-compose.yaml` for MySQL and Redis local development

## 12. Integration & E2E Tests

- [x] 12.1 Write integration test for full message flow (send message → agent orchestration → SSE stream → message persisted to all layers)
- [x] 12.2 Write integration test for parallel agent execution (verify interleaved SSE events from multiple agents)
- [x] 12.3 Write integration test for file operations (upload → agent writes via Xizhi → download)

## 13. API Documentation

- [x] 13.1 Create `api/openapi.yaml` — full OpenAPI 3.0 spec covering all endpoints, request/response schemas, error format, SSE event types
- [x] 13.2 Verify all API endpoints match OpenAPI spec
