## ADDED Requirements

### Requirement: Process role selection

系统 SHALL 允许通过 `serve` 子命令选择运行角色，取值为 `all`、`api`、`agent`，默认 `all`。角色决定该进程注册的路由子集、监听端口与日志文件名；`all` 角色保留拆分前的单进程行为。

#### Scenario: 默认角色为 all 以保持向后兼容
- **WHEN** 执行 `serve` 且未指定角色
- **THEN** 进程以 `all` 角色运行，注册全部路由、在 `server.port` 上启动单一 HTTP 监听，行为与拆分前的单体一致

#### Scenario: 显式选择 api 角色
- **WHEN** 执行 `serve --role api`
- **THEN** 进程以 `api` 角色运行，仅注册 API 侧路由，在 `server.port` 上监听

#### Scenario: 显式选择 agent 角色
- **WHEN** 执行 `serve --role agent`
- **THEN** 进程以 `agent` 角色运行，仅注册 agent 侧路由，在 `server.agent_port` 上监听

#### Scenario: 非法角色被拒绝
- **WHEN** 执行 `serve --role foo`
- **THEN** 系统打印错误并以非零状态码退出，不启动服务

### Requirement: Role-scoped route ownership

系统 SHALL 按角色注册路由：`api` 角色注册鉴权/会话 CRUD/消息历史读取/标题更新/工作空间文件 CRUD/skills 列表等 CRUD 路由；`agent` 角色注册流式消息端点 `POST /api/v1/sessions/:session_id/messages` 与 MCP 工具列表 `GET /api/v1/mcp/tools`；`all` 角色注册两者之并集。

#### Scenario: api 角色不注册流式消息端点
- **WHEN** 以 `api` 角色启动
- **THEN** `POST /api/v1/sessions/:session_id/messages` 不被注册，对该端点的请求返回 404

#### Scenario: agent 角色不注册 CRUD 路由
- **WHEN** 以 `agent` 角色启动
- **THEN** 会话列表/创建、消息历史读取、工作空间文件 CRUD、skills 列表等 CRUD 路由均不被注册，对这些端点的请求返回 404

#### Scenario: MCP 工具列表由 agent 角色提供
- **WHEN** 以 `agent` 角色启动
- **THEN** `GET /api/v1/mcp/tools` 被注册并返回已发现的 MCP 工具

#### Scenario: all 角色注册全部路由
- **WHEN** 以 `all` 角色启动
- **THEN** 上述 CRUD 路由与流式/MCP 路由全部被注册，与拆分前一致

### Requirement: Per-role HTTP listener

`api` 角色 SHALL 在 `server.port` 上启动 HTTP 服务；`agent` 角色 SHALL 在 `server.agent_port` 上启动 HTTP 服务；`all` 角色 SHALL 在 `server.port` 上启动单一 HTTP 服务并注册全部路由。每个角色的 HTTP 服务各自独立进行 graceful shutdown。

#### Scenario: api 角色监听 server.port
- **WHEN** 以 `api` 角色启动且 `server.port` 为 8080
- **THEN** HTTP 服务监听 0.0.0.0:8080

#### Scenario: agent 角色监听 server.agent_port
- **WHEN** 以 `agent` 角色启动且 `server.agent_port` 为 8081
- **THEN** HTTP 服务监听 0.0.0.0:8081

#### Scenario: all 角色在 server.port 上单监听
- **WHEN** 以 `all` 角色启动
- **THEN** 仅在 `server.port` 上启动一个 HTTP 服务（无第二监听），全部路由经此服务对外

#### Scenario: agent 角色端口可独立配置
- **WHEN** `server.agent_port` 未在配置中指定
- **THEN** 系统使用默认 agent 端口（如 8081）或启动时报错提示必须显式配置，二选一由实现决定并在日志中说明

### Requirement: Shared runtime data root

`api` 与 `agent` 角色 SHALL 从同一个 `-d`/`--data-dir` 派生 `data`、`logs`、`skills`、`tools` 四类落盘位置，并连接同一份 MySQL、Redis 与本地文件系统存储；两角色之间不存在数据面隔离，三层持久化、xizhi 工作空间工具、executor 沙箱与 Landlock 策略的行为均与单进程时一致。

#### Scenario: 两角色共享同一数据目录
- **WHEN** `api` 与 `agent` 角色以相同的 `-d /var/lib/blowball` 启动
- **THEN** 两者读写相同的 `/var/lib/blowball/data`、`/var/lib/blowball/logs`、`/var/lib/blowball/skills`、`/var/lib/blowball/tools`

#### Scenario: 两角色共享同一存储后端
- **WHEN** 两角色同时运行
- **THEN** 两者连接配置中同一 MySQL 与 Redis；agent 角色写入的 turn 结果可被 api 角色读取

### Requirement: Role-aware log file

系统 SHALL 按角色命名 `{data-dir}/logs/` 下的日志文件：`api` 角色写入 `blowball-api.log`，`agent` 角色写入 `blowball-agent.log`，`all` 角色写入 `blowball.log`，以避免两个进程的 lumberjack 实例争用同一文件。

#### Scenario: api 角色日志文件名
- **WHEN** 以 `api` 角色启动且 `logging.output` 包含文件
- **THEN** 在 `{data-dir}/logs/` 下产生并写入 `blowball-api.log`

#### Scenario: agent 角色日志文件名
- **WHEN** 以 `agent` 角色启动且 `logging.output` 包含文件
- **THEN** 在 `{data-dir}/logs/` 下产生并写入 `blowball-agent.log`

#### Scenario: all 角色日志文件名
- **WHEN** 以 `all` 角色启动且 `logging.output` 包含文件
- **THEN** 在 `{data-dir}/logs/` 下产生并写入 `blowball.log`（与拆分前一致）

### Requirement: Fault isolation between roles

`api` 角色 SHALL 在运行期不依赖 `agent` 角色所在进程；`agent` 角色进程的崩溃、不可用或重启 SHALL NOT 阻止 `api` 角色继续提供其自有路由（鉴权、会话 CRUD、消息历史读取、工作空间文件 CRUD、skills 列表）。反之，`api` 角色进程不可用 SHALL NOT 阻止 `agent` 角色提供流式消息端点与 MCP 工具列表。

#### Scenario: agent 角色不可用时 api 路由仍可用
- **WHEN** `agent` 角色进程未运行或已崩溃，而 `api` 角色进程正常运行
- **THEN** 对 `api` 角色自有路由（如 `GET /api/v1/sessions`）的请求仍正常返回

#### Scenario: api 角色不依赖 agent 层
- **WHEN** 以 `api` 角色启动
- **THEN** 该进程不实例化 orchestrator、OpenAI 客户端、tool registry 或 executor 沙箱，且其正常运转不需要 `agent` 角色进程在线

### Requirement: Agent role owns the streaming-turn pipeline

`agent` 角色 SHALL 在自身进程内完成一次对话回合所需的全部步骤：会话查询、历史消息恢复（`RecoverMessages`）、orchestrator 执行、SSE 流式回写、turn 结束后的三层持久化（`SaveMessagesBatch`）以及首轮标题生成（`TitleService`）。

#### Scenario: 流式端点在 agent 角色内自洽
- **WHEN** 以 `agent` 角色启动并收到 `POST /api/v1/sessions/:session_id/messages` 请求
- **THEN** 会话查询、历史恢复、agent 执行、SSE 回写与持久化均在该 agent 进程内完成，不跨进程调用 `api` 角色

#### Scenario: 标题生成在 agent 角色内运行
- **WHEN** 首轮对话成功完成
- **THEN** 标题生成由 `agent` 角色进程中的 `TitleService` 异步触发并写入 MySQL，不依赖 `api` 角色进程
