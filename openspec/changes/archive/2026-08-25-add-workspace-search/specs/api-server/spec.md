## MODIFIED Requirements

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
  - GET /api/v1/sessions/:session_id
  - GET /api/v1/sessions/:session_id/messages

- **AND** 当且仅当角色为 `all` 时，额外注册 POST /api/v1/sessions/:session_id/messages（流式端点；`agent` 角色独立注册此路由）

#### Scenario: Workspace routes

- **WHEN** 服务以 `api` 或 `all` 角色启动
- **THEN** 注册以下需要鉴权的路由：
  - GET /api/v1/workspace/files
  - POST /api/v1/workspace/upload
  - GET /api/v1/workspace/files/*path
  - GET /api/v1/workspace/files/*path/content
  - GET /api/v1/workspace/search（工作区条目搜索；响应契约见 `workspace-api` 的 Search workspace entries 需求）

#### Scenario: Tool and skill routes

- **WHEN** 服务以 `api` 或 `all` 角色启动
- **THEN** 注册需要鉴权的 GET /api/v1/skills
- **AND** GET /api/v1/mcp/tools 由 `agent` 角色注册（`all` 角色亦注册）

## ADDED Requirements

### Requirement: Workspace search route

系统 SHALL 暴露 GET /api/v1/workspace/search 路由，用于按名字搜索工作区文件与目录（响应契约见 `workspace-api` 的 Search workspace entries 需求）。该路由位于鉴权路由组内，在 `/workspace/{search,files}` 静态节点分叉注册（与 `upload` 同款），不经由 `GET /workspace/files/*path` catch-all 派发。

#### Scenario: Route is authenticated

- **WHEN** 服务以 `api` 或 `all` 角色启动
- **THEN** GET /api/v1/workspace/search 位于鉴权路由组内，未携带有效 token 时返回 401

#### Scenario: Route is not registered by the agent role

- **WHEN** 服务以 `agent` 角色启动
- **THEN** GET /api/v1/workspace/search 不被注册，对该端点的请求返回 404
