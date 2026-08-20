# service-roles 变更规格

> 注：本 delta 的 Role-scoped route ownership 基于 turn-detach-resume 变更后的版本叠加（含其 turn 生命周期路由）；归档顺序必须 turn-detach-resume 在前。

## MODIFIED Requirements

### Requirement: Role-scoped route ownership

系统 SHALL 按角色注册路由：`api` 角色注册鉴权/会话 CRUD/消息历史读取/标题更新/工作空间文件 CRUD/skills 列表/模型列表等 CRUD 路由；`agent` 角色注册流式消息端点 `POST /api/v1/sessions/:session_id/messages`、turn 生命周期端点（`POST /api/v1/sessions/:session_id/turns/:run_id/cancel` 与 `GET /api/v1/sessions/:session_id/turns/:run_id/events`）以及 MCP 工具列表 `GET /api/v1/mcp/tools`；`all` 角色注册两者之并集。

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
- **THEN** 上述 CRUD 路由（含模型列表）与流式/turn 生命周期/MCP 路由全部被注册，无重复注册冲突
