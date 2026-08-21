# Tasks: add-session-get

## 1. Service 层

- [x] 1.1 在 `internal/service/session.go` 新增 `GetSessionDetail`：组合 `GetSessionByID` 与 title 读取（`GetTitle`），返回 session 行 + title 字符串；title 行缺失 → 空字符串，title 读取出错 → WARN 日志 + 空字符串降级（不失败请求）；session 查询出错原样返回错误

## 2. Handler 与路由

- [x] 2.1 在 `internal/handler/session.go` 新增 `getSessionResponse` 结构（`session_id`/`title`/`create_time`/`update_time` RFC3339 UTC/`generating`/`run_id,omitempty`）与 `SessionHandler.GetSession` 方法：`GetSessionDetail` 取数 → 所有权校验（`sess == nil || sess.UserID != userID` → 404）→ `runStore.ActiveRuns([sessionID])` 单元素调用取 `generating`/`run_id`，claim 读取失败 WARN + 降级 `generating=false`，`runStore == nil` 固定 false
- [x] 2.2 在 `internal/handler/router.go` 的 `RouteDeps` 增加 `SessionGet` 字段，`RegisterAPIRoutes` 的 sessions 组注册 `authed.GET("/sessions/:session_id", deps.SessionGet)`（api 分区；agent 角色不注册）
- [x] 2.3 在 `cmd/blowball/serve.go` 的 `wireAPI` 接线 `SessionGet: sessionHandler.GetSession`

## 3. 测试

- [x] 3.1 `internal/handler/session_test.go` 新增 GetSession 用例：200 响应字段完整（含 `create_time`）；非所有者 404；不存在 404；有活跃 run 时 `generating:true` + `run_id`；`runStore == nil` 时 `generating:false`；title 读取失败降级空 title
- [x] 3.2 `test/integration/role_ownership_test.go`：api/all 分区断言加入 `GET /api/v1/sessions/:session_id`，agent 分区断言其不被注册

## 4. 契约与文档

- [x] 4.1 `api/openapi.yaml` 在 `/api/v1/sessions/{session_id}` 下新增 `get` 操作（200 响应 schema 含全部字段、401/404/500），并把 `api/openapi.yaml` 同步到 `blowball-frontend` 重新生成（`npm run generate-api`）
- [x] 4.2 更新 `CLAUDE.md` 端点表：`GET /api/v1/sessions/:session_id` 行（api partition，单会话详情）
- [x] 4.3 `make test` + `make lint` 全绿
