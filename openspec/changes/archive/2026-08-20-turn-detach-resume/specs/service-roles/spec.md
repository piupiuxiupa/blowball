# service-roles 变更规格

## MODIFIED Requirements

### Requirement: Role-scoped route ownership

系统 SHALL 按角色注册路由：`api` 角色注册鉴权/会话 CRUD/消息历史读取/标题更新/工作空间文件 CRUD/skills 列表等 CRUD 路由；`agent` 角色注册流式消息端点 `POST /api/v1/sessions/:session_id/messages`、turn 生命周期端点（`POST /api/v1/sessions/:session_id/turns/:run_id/cancel` 与 `GET /api/v1/sessions/:session_id/turns/:run_id/events`）以及 MCP 工具列表 `GET /api/v1/mcp/tools`；`all` 角色注册两者之并集。

#### Scenario: api 角色不注册流式消息端点
- **WHEN** 以 `api` 角色启动
- **THEN** `POST /api/v1/sessions/:session_id/messages` 不被注册，对该端点的请求返回 404

#### Scenario: api 角色不注册 turn 生命周期端点
- **WHEN** 以 `api` 角色启动
- **THEN** `POST /api/v1/sessions/:session_id/turns/:run_id/cancel` 与 `GET /api/v1/sessions/:session_id/turns/:run_id/events` 不被注册，对这两个端点的请求返回 404

#### Scenario: agent 角色不注册 CRUD 路由
- **WHEN** 以 `agent` 角色启动
- **THEN** 会话列表/创建、消息历史读取、工作空间文件 CRUD、skills 列表等 CRUD 路由均不被注册，对这些端点的请求返回 404

#### Scenario: turn 生命周期端点由 agent 角色提供
- **WHEN** 以 `agent` 角色启动
- **THEN** `POST /api/v1/sessions/:session_id/turns/:run_id/cancel` 与 `GET /api/v1/sessions/:session_id/turns/:run_id/events` 被注册并可处理请求

#### Scenario: MCP 工具列表由 agent 角色提供
- **WHEN** 以 `agent` 角色启动
- **THEN** `GET /api/v1/mcp/tools` 被注册并返回已发现的 MCP 工具

#### Scenario: all 角色注册全部路由
- **WHEN** 以 `all` 角色启动
- **THEN** 上述 CRUD 路由与流式/turn 生命周期/MCP 路由全部被注册，与拆分前一致

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
