## MODIFIED Requirements

### Requirement: Role-scoped route ownership

系统 SHALL 按角色注册路由：`api` 角色注册鉴权/会话 CRUD/消息历史读取/标题更新/工作空间文件 CRUD/skills 列表/模型列表等 CRUD 路由，以及定时任务管理端点（`POST /api/v1/scheduled-tasks`、`GET /api/v1/scheduled-tasks`、`GET /api/v1/scheduled-tasks/:task_id`、`PATCH /api/v1/scheduled-tasks/:task_id`、`DELETE /api/v1/scheduled-tasks/:task_id`）和运行历史端点 `GET /api/v1/scheduled-tasks/:task_id/runs`；`agent` 角色注册流式消息端点 `POST /api/v1/sessions/:session_id/messages`、turn 生命周期端点（`POST /api/v1/sessions/:session_id/turns/:run_id/cancel` 与 `GET /api/v1/sessions/:session_id/turns/:run_id/events`）以及 MCP 工具列表 `GET /api/v1/mcp/tools`；`all` 角色注册两者之并集。

#### Scenario: api 角色不注册流式消息端点
- **WHEN** 以 `api` 角色启动
- **THEN** `POST /api/v1/sessions/:session_id/messages` 不被注册，对该端点的请求返回 404

#### Scenario: api 角色不注册 turn 生命周期端点
- **WHEN** 以 `api` 角色启动
- **THEN** `POST /api/v1/sessions/:session_id/turns/:run_id/cancel` 与 `GET /api/v1/sessions/:session_id/turns/:run_id/events` 不被注册，对这两个端点的请求返回 404

#### Scenario: 模型列表由 api 角色提供
- **WHEN** 以 `api` 角色启动
- **THEN** `GET /api/v1/models` 被注册并返回模型目录；`agent` 角色不注册该端点（对 agent 端口的请求返回 404）

#### Scenario: agent 角色不注册 CRUD 路由
- **WHEN** 以 `agent` 角色启动
- **THEN** 会话列表/创建、消息历史读取、工作空间文件 CRUD、skills 列表、模型列表等 CRUD 路由均不被注册，对这些端点的请求返回 404

#### Scenario: turn 生命周期端点由 agent 角色提供
- **WHEN** 以 `agent` 角色启动
- **THEN** `POST /api/v1/sessions/:session_id/turns/:run_id/cancel` 与 `GET /api/v1/sessions/:session_id/turns/:run_id/events` 被注册并可处理请求

#### Scenario: MCP 工具列表由 agent 角色提供
- **WHEN** 以 `agent` 角色启动
- **THEN** `GET /api/v1/mcp/tools` 被注册并返回已发现的 MCP 工具

#### Scenario: all 角色注册全部路由
- **WHEN** 以 `all` 角色启动
- **THEN** 上述 CRUD 路由（含模型列表、定时任务管理和运行历史）与流式/turn 生命周期/MCP 路由全部被注册，无重复注册冲突

#### Scenario: api 角色管理定时任务
- **WHEN** 以 `api` 角色启动且数据库迁移已就绪
- **THEN** 定时任务管理及运行历史路由已注册，并按用户身份提供服务
- **AND** 不要求 Agent 进程在线

#### Scenario: agent 角色不注册任务管理路由
- **WHEN** 以 `agent` 角色启动
- **THEN** 所有定时任务管理和运行历史 HTTP 路由均不注册，请求返回 404

## ADDED Requirements

### Requirement: Scheduled task runtime ownership

系统 SHALL 仅在已启用调度的 `agent / all` 角色启动后台调度与恢复模块，该模块 SHALL 共用本进程的 Turn 执行入口、Run registry 和既有 Agent 实现。`api` 角色 SHALL 不启动调度或任务执行，不构造 LLM 客户端、Orchestrator、工具注册表或沙箱。第一版 SHALL 保留 `all / api / agent` 三种角色及现有监听边界，不要求新增服务或运行角色。

#### Scenario: Agent role executes without an API process
- **WHEN** 调度已启用、任务已保存，只有 Agent 角色在线
- **THEN** Agent 角色从 MySQL 领取并执行任务，不跨进程调用 API 角色

#### Scenario: API role remains lightweight
- **WHEN** 只有 API 角色在线
- **THEN** 任务管理和运行历史仍可使用，但不启动后台 Agent 执行或加载 Agent 运行时依赖

#### Scenario: Combined role starts one scheduler
- **WHEN** 以 all 角色启动且调度已启用
- **THEN** 该进程只有一个调度模块实例，HTTP 及定时入口共享同一套 Turn 执行代码
