# service-roles Specification

## Purpose

定义后端服务进程角色（`all`、`api`、`agent`）的划分与职责，包括角色选择、路由归属、独立 HTTP 监听、共享运行时数据根、按角色命名的日志文件、角色间故障隔离，以及流式对话回合管线在 `agent` 角色内的归属。

## Requirements

### Requirement: Process role selection

主服务 SHALL 保留 serve 的 all/api/agent 角色，默认 all，现有 HTTP 端口和路由分区不变。all SHALL 合并 API/Agent 能力，但 SHALL NOT 内嵌 Scheduler。独立 Scheduler SHALL 使用 services/scheduler 下的独立入口，不作为主服务的新 role 或兼容 fallback。

#### Scenario: 默认角色为 all 以保持向后兼容
- **WHEN** serve 未指定角色
- **THEN** 以 all 注册现有 API/Agent HTTP 并在 server.port 单监听，不启动调度模块

#### Scenario: 显式选择 api 角色
- **WHEN** serve --role api
- **THEN** 只注册 API HTTP 分区，在 server.port 监听，内部提供非 LLM 能力

#### Scenario: 显式选择 agent 角色
- **WHEN** serve --role agent
- **THEN** 只注册 Agent HTTP 分区，在 server.agent_port 监听，并提供任务执行内部 RPC

#### Scenario: 非法角色被拒绝
- **WHEN** serve --role foo 或把 scheduler 当作主服务 role
- **THEN** 非零退出，不启动服务

### Requirement: Role-scoped route ownership

API SHALL 保留鉴权/普通会话 CRUD/消息历史/标题/工作空间/skills/模型等 CRUD，任务 HTTP 管理和运行历史改经 Scheduler RPC，新增任务会话/资源只读或管理接口。Agent SHALL 保留普通流式消息、turn 取消/事件恢复、MCP 工具列表，并提供任务 Run 显式取消；SHALL 不注册任务 CRUD/历史 HTTP。all SHALL 合并两组，不重复注册。

#### Scenario: api 角色不注册流式消息端点
- **WHEN** 以 api 启动
- **THEN** 普通 POST /api/v1/sessions/:session_id/messages 返回 404

#### Scenario: api 角色不注册 turn 生命周期端点
- **WHEN** 以 api 启动
- **THEN** 普通 turn cancel/events 和任务 Run cancel 不注册，返回 404

#### Scenario: 模型列表由 api 角色提供
- **WHEN** api 在线
- **THEN** GET /api/v1/models 提供目录，agent 端口返回 404

#### Scenario: agent 角色不注册 CRUD 路由
- **WHEN** 以 agent 启动
- **THEN** 普通/任务会话列表历史、工作空间及模型 CRUD 均不注册

#### Scenario: turn 生命周期端点由 agent 角色提供
- **WHEN** 以 agent 启动
- **THEN** 现有普通 cancel/events 和任务 Run 显式取消可用

#### Scenario: MCP 工具列表由 agent 角色提供
- **WHEN** 以 agent 启动
- **THEN** GET /api/v1/mcp/tools 仍可用

#### Scenario: all 角色注册全部路由
- **WHEN** 以 all 启动
- **THEN** API/Agent 路由合并，包含任务管理/历史/资源及取消，无冲突

#### Scenario: api 角色管理定时任务
- **WHEN** API 与 Scheduler 可用但 Agent 离线
- **THEN** 不需要执行的任务管理/历史功能可用，主服务不启动本地 Agent 或调度

#### Scenario: agent 角色不注册任务管理路由
- **WHEN** 请求 agent 的任务 CRUD/运行历史 HTTP
- **THEN** 返回 404；其 Agent 管理工具使用内部 RPC

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

`api` 与 `agent` 角色 SHALL 从同一个 `-d`/`--data-dir` 派生 `data`、`logs`、`skills`、`tools`、`skills-market` 五类落盘位置，并连接同一份 MySQL、Redis 与共享 POSIX 文件系统存储；两角色之间不存在数据面隔离，三层持久化、xizhi 工作空间工具、executor 沙箱与 Landlock 策略的行为均与单进程时一致。该共享 POSIX 文件系统默认为本地磁盘；当配置为共享模式（见 `workspace-shared-storage`，实现为 MinIO 支撑的 JuiceFS 挂载）时，`data` 子树 SHALL 由 operator 在进程启动前挂载就绪，使跨机器的 `api`/`agent` 实例读写同一份 per-user 数据；`skills-market` 子树不参与该共享模式检查，由 operator 在每台主机各自同步维护（同 `tools` 约定）。当 `skill_market.url` 启用时，`api` 角色 SHALL 为 skills 列表端点构造市场客户端（仅 HTTP 客户端装配，不引入 orchestrator/OpenAI 依赖），`agent` 角色 SHALL 为 luban 工具与 executor 沙箱构造同一客户端。

#### Scenario: 两角色共享同一数据目录
- **WHEN** `api` 与 `agent` 角色以相同的 `-d /var/lib/blowball` 启动
- **THEN** 两者读写相同的 `/var/lib/blowball/data`、`/var/lib/blowball/logs`、`/var/lib/blowball/skills`、`/var/lib/blowball/tools`、`/var/lib/blowball/skills-market`

#### Scenario: 两角色共享同一存储后端
- **WHEN** 两角色同时运行
- **THEN** 两者连接配置中同一 MySQL 与 Redis；agent 角色写入的 turn 结果可被 api 角色读取

#### Scenario: 跨机器实例经共享文件系统共享数据面
- **WHEN** `storage.workspace.backend` 为 `shared`，且分别位于两台机器的 `api` 与 `agent` 实例将 JuiceFS 挂载到各自的 `{data-dir}/data`（指向同一 MinIO bucket + 同一元数据引擎）
- **THEN** 一台机器上对 `{data-dir}/data/{userID}/` 的写对另一台机器可见，工作空间与 per-user 数据跨机共享，不存在数据面隔离

#### Scenario: api 角色独立提供含市场条目的 skills 列表
- **WHEN** `skill_market.url` 已启用，仅 `api` 角色运行，用户请求 `GET /api/v1/skills`
- **THEN** api 角色以该请求的 JWT 拉取市场清单并返回合并结果，不依赖 `agent` 角色进程

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

`agent` 角色 SHALL 在自身进程内完成一次对话回合所需的全部步骤：会话查询、历史消息恢复（`RecoverMessages`）、orchestrator 执行、SSE 流式回写、turn 结束后的三层持久化（`SaveMessagesBatch`）以及首轮标题生成（`TitleService`）。turn 的运行注册表（run registry）与 Redis run 状态 SHALL 由 `agent` 角色进程持有与维护；取消端点与恢复端点 SHALL 在 `agent` 角色进程内基于该注册表与 Redis run 状态工作。

#### Scenario: 流式端点在 agent 角色内自洽
- **WHEN** 以 `agent` 角色启动并收到 `POST /api/v1/sessions/:session_id/messages` 请求
- **THEN** 会话查询、历史恢复、agent 执行、SSE 回写与持久化均在该 agent 进程内完成，不跨进程调用 `api` 角色

#### Scenario: turn 生命周期端点在 agent 角色内自洽
- **WHEN** 以 `agent` 角色启动并收到取消或恢复端点请求
- **THEN** registry 查询、Redis run 状态读写与（取消路径的）部分持久化均在 agent 进程内或经共享 Redis 完成，不调用 `api` 角色

#### Scenario: 标题生成在 agent 角色内运行
- **WHEN** 首轮对话成功完成
- **THEN** 标题生成由 `agent` 角色进程中的 `TitleService` 异步触发并写入 MySQL，不依赖 `api` 角色进程

### Requirement: Scheduled task runtime ownership

Scheduler SHALL 独立承担后台调度与恢复；all/api/agent SHALL 均不启动该模块。Agent/all SHALL 通过内部执行 RPC 共用现行 turn/Run registry/Agent 处理任务，并持有执行凭证/时限；API/all SHALL 只提供非 LLM 内部及公共管理/历史能力，不构造 Agent 依赖。跨服务任务管理、启动授权和资源引用 SHALL 经内部 RPC。

#### Scenario: Agent role executes without an API process
- **WHEN** Agent 在线但所需 API 身份/资源校验不可用
- **THEN** 不能绕过校验启动新任务，不回退内嵌调度；已接收任务按可靠状态和既定时限处理

#### Scenario: API role remains lightweight
- **WHEN** 只有 API 与 Scheduler 在线
- **THEN** 管理/历史可用，不运行模型或加载 Agent

#### Scenario: Combined role starts one scheduler
- **WHEN** all 启动
- **THEN** 内嵌调度实例数量为零；调度必须由独立 Scheduler 提供，执行仍共用 turn 代码

### Requirement: Role-scoped internal RPC listeners

API/Agent SHALL 按角色提供独立配置的内部 RPC 能力；all 可以在同一内部监听器合并两组。内部监听 SHALL 不改变公共 HTTP 角色/端口，并遵循 mTLS/服务身份授权。缺少 Scheduler endpoint 或 Scheduler 故障 SHALL 不阻止普通主服务启动；旧启用内嵌调度配置 SHALL 给出明确迁移错误/指引，不能启动第二调度器。

#### Scenario: Scheduler is offline at main service startup
- **WHEN** Scheduler 暂时不可达
- **THEN** 普通主服务可启动；任务调用明确不可用，无本地 fallback

#### Scenario: Legacy embedded scheduling configuration remains
- **WHEN** 主服务仍配置旧 scheduled_tasks.enabled=true
- **THEN** 明确提示迁移且不启动内嵌实例，禁止静默双调度
