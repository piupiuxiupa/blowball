# Proposal: add-session-get

## Why

目前获取单个会话的状态（标题、generating、活跃 run_id）只能通过 `GET /api/v1/sessions` 拉取该用户的全部会话列表再在前端过滤。会话数量增长后，单会话状态轮询（如等待一个 turn 结束、重载后恢复 attach 目标）都要付出全量列表的代价。需要一个单会话读取端点。

## What Changes

- 新增 `GET /api/v1/sessions/:session_id` 单会话详情端点（鉴权，归属 `api` 角色的 CRUD 路由分区）。
- 响应体是现有列表条目（`sessionListEntry`）的超集：`session_id` / `title` / `create_time`（新增量字段）/ `update_time` / `generating` / `run_id`（仅 generating 时出现）。
- 所有权语义与现有会话端点一致：不存在或属于其他用户一律 404，不泄露他人会话的存在性。
- `api/openapi.yaml` 在 `/api/v1/sessions/{session_id}` 路径下补 `get` 操作；前端仓库随后重新生成 API 类型。
- 不修改任何现有端点行为；无数据库变更。

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `session-management`: 在既有 "Session list" 需求旁新增 "Session detail" 需求 —— 单会话详情读取、响应字段契约（含 `create_time` 增量与 `generating`/`run_id` 活跃 run 标记）、404 所有权语义。
- `api-server`: "API routing" 需求的 Session routes 清单加入 `GET /api/v1/sessions/:session_id`（`api` 与 `all` 角色注册），并新增对应的 "Session detail route" 鉴权场景。

## Impact

- `internal/handler/session.go` — `SessionHandler` 新增 `GetSession` 处理方法。
- `internal/handler/router.go` — `RouteDeps` 新增 `SessionGet` 字段；`RegisterAPIRoutes` 注册 `GET /sessions/:session_id`。
- `cmd/blowball/serve.go` — `wireAPI` 接线新 handler 方法。
- `internal/service/session.go`（可能）— 组合 session 行 + title 的单会话读取（`GetSessionByID` 不返回 title，需要与 `GetTitle` 组合或新增带 title 的读取）。
- `internal/handler/run`（`ActiveRuns`）— 复用现有单/批量活跃 run 查询取 `generating`/`run_id`。
- `test/integration/role_ownership_test.go` — api 分区路由断言加入新端点。
- `api/openapi.yaml` — 新增 `get` 操作定义；`blowball-frontend` 同步再生成。
