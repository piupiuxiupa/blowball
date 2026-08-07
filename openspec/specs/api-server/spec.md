# api-server Specification

## Purpose

定义后端 HTTP API 服务能力，包括 Gin HTTP 服务、CORS 中间件、统一错误响应、API 路由注册、trace_id 请求上下文、结构化日志以及 graceful shutdown。

## Requirements

### Requirement: Gin HTTP server
系统 SHALL 使用 Gin 框架为当前角色启动 HTTP 服务，监听端口由角色决定：`api` 角色与 `all` 角色监听 `server.port`，`agent` 角色监听 `server.agent_port`。

#### Scenario: Server starts on configured port
- **WHEN** 服务以 `api` 或 `all` 角色启动，config.yaml 中 server.port 为 8080
- **THEN** HTTP 服务监听 0.0.0.0:8080

#### Scenario: Agent role starts on agent port
- **WHEN** 服务以 `agent` 角色启动，config.yaml 中 server.agent_port 为 8081
- **THEN** HTTP 服务监听 0.0.0.0:8081

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
系统 SHALL 按当前角色注册对应的 API 路由组（角色与路由的对应关系见 `service-roles`）：`all` 角色注册下列全部路由，`api` 角色注册其中的 CRUD 子集，`agent` 角色仅注册流式消息端点与 MCP 工具列表。下列路由清单描述 `all` 角色注册的完整目录。

#### Scenario: Auth routes
- **WHEN** 服务以注册 auth 路由的角色（`api`、`all`）启动
- **THEN** 注册 POST /api/v1/auth/login（无需鉴权）

#### Scenario: Session routes
- **WHEN** 服务以 `api` 或 `all` 角色启动
- **THEN** 注册以下需要鉴权的路由：
  - GET /api/v1/sessions
  - POST /api/v1/sessions
  - GET /api/v1/sessions/:session_id/messages

- **AND** 当且仅当角色为 `all` 时，额外注册 POST /api/v1/sessions/:session_id/messages（流式端点；`agent` 角色独立注册此路由）

#### Scenario: Workspace routes
- **WHEN** 服务以 `api` 或 `all` 角色启动
- **THEN** 注册以下需要鉴权的路由：
  - GET /api/v1/workspace/files
  - POST /api/v1/workspace/upload
  - GET /api/v1/workspace/files/*path
  - GET /api/v1/workspace/files/*path/content

#### Scenario: Tool and skill routes
- **WHEN** 服务以 `api` 或 `all` 角色启动
- **THEN** 注册需要鉴权的 GET /api/v1/skills
- **AND** GET /api/v1/mcp/tools 由 `agent` 角色注册（`all` 角色亦注册）

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

### Requirement: Request context with trace_id
系统 SHALL 为每个 HTTP 请求生成唯一 trace_id，贯穿整个请求链路。

#### Scenario: Trace ID generated per request
- **WHEN** 任何 API 请求到达
- **THEN** 中间件生成 UUID v7 格式的 trace_id，写入 gin.Context，传递到 service、agent、store 各层

#### Scenario: Trace ID in logs
- **WHEN** 请求处理过程中记录日志
- **THEN** 日志包含 trace_id 字段，可按 trace_id 追踪完整请求链路

### Requirement: Structured logging
系统 SHALL 使用 zap 结构化日志，关键操作必须记录日志。日志编码格式由 `logging.format` 控制（默认 `json`，可选 `console`）；日志默认同时输出到控制台与文件（见“Log file persistence and rotation”）。

#### Scenario: Log format defaults to JSON
- **WHEN** 服务运行且未设置 `logging.format` 或将其设为 `json`
- **THEN** 日志使用 JSON 格式，包含 timestamp、level、trace_id、message 等字段

#### Scenario: Console format
- **WHEN** 配置 `logging.format: console`
- **THEN** 日志改用 console 编码格式输出，仍包含 timestamp、level、trace_id、message 等字段

#### Scenario: Key operation logging
- **WHEN** 以下操作发生时
- **THEN** 记录日志：用户登录、Agent 调用开始/结束、tool 调用、消息存储写入、错误发生

### Requirement: Graceful shutdown
系统 SHALL 支持 graceful shutdown，收到 SIGTERM/SIGINT 时优雅退出。

#### Scenario: Graceful shutdown on signal
- **WHEN** 服务收到 SIGTERM 或 SIGINT 信号
- **THEN** 系统停止接收新请求，等待进行中的请求完成（最长 10 秒），然后关闭数据库和 Redis 连接

### Requirement: Deletion routes
系统 SHALL 在鉴权路由组内注册会话删除与工作空间删除两条路由。

#### Scenario: Session delete route
- **WHEN** 服务启动
- **THEN** 注册需要鉴权的 `DELETE /api/v1/sessions/:session_id`

#### Scenario: Workspace delete route
- **WHEN** 服务启动
- **THEN** 注册需要鉴权的 `DELETE /api/v1/workspace/files/*path`

#### Scenario: Deletion routes require auth
- **WHEN** 未携带有效 token 访问任一删除路由
- **THEN** 返回 HTTP 401

### Requirement: Command-line interface
系统 SHALL 通过 cobra 提供统一 CLI，包含 `serve` 与 `seed` 两个子命令；`-f`/`--config`（配置文件路径，默认 `config.yaml`）与 `-d`/`--data-dir`（运行时数据根目录，默认当前工作目录）为在根命令上定义、两个子命令均可用的持久化标志。`serve` 子命令 SHALL 额外接受 `--role` 本地标志，取值 `all`（默认）、`api`、`agent`，用于选择进程运行角色（见 `service-roles`）。

#### Scenario: serve 子命令启动 HTTP 服务
- **WHEN** 执行 `serve`（可选附带 `-f`/`-d`）
- **THEN** 系统按配置启动 Gin HTTP 服务，行为与原 `cmd/server` 一致（含路由注册、graceful shutdown）

#### Scenario: seed 子命令创建用户
- **WHEN** 执行 `seed -username alice`（可选附带 `-password`、`-f`、`-d` 等）
- **THEN** 系统创建用户，行为与原 `bin/seed` 一致

#### Scenario: 指定配置文件路径
- **WHEN** 执行 `serve -f /etc/blowball/config.yaml`
- **THEN** 系统从 `/etc/blowball/config.yaml` 加载配置

#### Scenario: 持久化标志默认值
- **WHEN** 未传入 `-f` 或 `-d`
- **THEN** 配置文件路径默认为当前工作目录下的 `config.yaml`，数据根目录默认为当前工作目录

#### Scenario: 无子命令或未知标志打印帮助并以非零码退出
- **WHEN** 不带任何子命令执行，或传入未定义的标志
- **THEN** 系统打印 cobra 帮助信息并以非零状态码退出，不启动服务

#### Scenario: 通过 --role 选择运行角色
- **WHEN** 执行 `serve --role agent`
- **THEN** 系统以 `agent` 角色启动，仅注册 agent 侧路由并在 `server.agent_port` 监听

### Requirement: Runtime data root
系统 SHALL 从 `-d`/`--data-dir` 指定的单一运行时根目录派生四类落盘位置：每用户数据 `{data-dir}/data`、日志文件 `{data-dir}/logs`、全局 skills `{data-dir}/skills`、操作员工具目录 `{data-dir}/tools`；若根目录或所需子目录不存在，则 SHALL 在启动时创建。`{data-dir}/tools` 用于存放操作者为沙箱内 `bash` 工具提供的 CLI 二进制（`python`/`pip_install` 专用执行器已移除），将在沙箱内以只读方式挂载到 `$HOME/.local/bin`。

#### Scenario: 默认根解析到当前工作目录
- **WHEN** 未指定 `-d`
- **THEN** 数据根为当前工作目录，四类路径分别解析为 `./data`、`./logs`、`./skills`、`./tools`（与历史布局一致，新增 `./logs` 与 `./tools`）

#### Scenario: 自定义根重新定位四类状态
- **WHEN** 执行 `serve -d /var/lib/blowball`
- **THEN** 每用户数据、日志、全局 skills、操作者工具分别写入 `/var/lib/blowball/data`、`/var/lib/blowball/logs`、`/var/lib/blowball/skills`、`/var/lib/blowball/tools`

#### Scenario: 自动创建缺失目录
- **WHEN** 指定的根目录或其子目录尚不存在
- **THEN** 系统在启动时创建这些目录（权限 0o755），包括 `{data-dir}/tools`

#### Scenario: 操作者工具目录始终被创建
- **WHEN** 服务启动且 `{data-dir}/tools` 尚不存在
- **THEN** 系统创建 `{data-dir}/tools`（即使其为空），以便 Landlock 规则与沙箱挂载始终可解析
- **AND** 启动布局日志中包含 `tools_dir` 字段

### Requirement: Log file persistence and rotation
系统 SHALL 将结构化日志写入 `{data-dir}/logs/` 下的文件，并按 `logging.file` 配置（`max_size_mb`、`max_backups`、`max_age_days`、`compress`）经 lumberjack 进行轮转；输出目标由 `logging.output`（默认同时包含控制台与文件）控制。

#### Scenario: 日志写入文件
- **WHEN** 服务运行
- **THEN** 在 `{data-dir}/logs/` 下产生日志文件并持续写入结构化日志

#### Scenario: 控制台与文件双写
- **WHEN** `logging.output` 为默认值（同时包含控制台与文件）
- **THEN** 每条日志同时出现在控制台（stderr）与 `{data-dir}/logs/` 下的文件中

#### Scenario: 按大小轮转
- **WHEN** 日志文件大小超过 `logging.file.max_size_mb`
- **THEN** 触发轮转：当前文件被重命名（并在 `compress: true` 时压缩），新日志继续写入同名新文件

#### Scenario: 限制保留备份数
- **WHEN** 轮转后保留的备份文件数超过 `logging.file.max_backups`
- **THEN** 最旧的备份文件被删除

#### Scenario: 可禁用文件输出
- **WHEN** `logging.output` 仅包含控制台（不含文件）
- **THEN** 系统不创建或写入日志文件，仅向控制台输出

#### Scenario: 日志目录自动创建
- **WHEN** 启动时 `{data-dir}/logs/` 不存在
- **THEN** 系统在初始化日志器之前创建该目录
