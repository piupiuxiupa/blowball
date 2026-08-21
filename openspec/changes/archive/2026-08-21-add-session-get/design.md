# Design: add-session-get

## Context

`GET /api/v1/sessions/:session_id` 路径已存在于 gin 路由树（PATCH 标题、DELETE 会话挂在同一路径），gin 按方法路由，新增 GET 零冲突。现成零件：

- `SessionService.GetSessionByID`（`internal/service/session.go:93`）→ `mysql.Store.GetSessionByID`：返回完整 `model.Session`（含 `create_time`、`context_compacted`），missing 返回 `(nil, nil)`。
- `mysql.Store.GetTitle`（`internal/store/mysql/title.go:68`）：单会话标题查询；`GetSessionByID` 不带 title，list 走的是 `ListSessionsWithTitle` join。
- `run.Store.ActiveRuns`（`internal/handler/session.go:236` 已在用）：会话 → 活跃 run_id 映射，支撑 `generating` / `run_id`。
- 所有权校验模式（`internal/handler/session.go:131`）：`sess == nil || sess.UserID != userID` → 404。

## Goals / Non-Goals

**Goals:**

- 提供单会话详情读取端点，响应为列表条目的超集，前端两处可共用类型。
- 分区归属 `api`（CRUD 读），与 `SessionList` 并排注册。
- 所有权语义与现有会话端点逐字一致（404 不泄露存在性）。

**Non-Goals:**

- 不返回消息内容/分页（已有 `GET .../messages`）。
- 不暴露 `context_compacted`（内部缝合标记，无前端消费场景）。
- 不做任何数据库/缓存变更。

## Decisions

### D1: 响应形状 —— 列表条目超集 + `create_time`

```json
{
  "session_id": "…",
  "title": "…",
  "create_time": "2026-08-21T…",
  "update_time": "2026-08-21T…",
  "generating": true,
  "run_id": "…"
}
```

- 时间字段与 list/PATCH 一律 `RFC3339` UTC 字符串。
- `run_id` 带 `omitempty`，仅 generating 时出现 —— 对齐 `sessionListEntry`。
- **备选**：仅返回 list 已有字段（不加 `create_time`）。否决：单条端点是暴露 `create_time` 的自然位置，model 里现成，前端会话详情页大概率要展示创建时间。

### D2: title 获取 —— handler 内组合两次读取，不加 store join

`GetSessionByID` + `GetTitle` 两次单行查询（handler 或 service 层组合），不新增 `GetSessionWithTitle` join 方法。

理由：这是低频、非热路径的端点；两次主键/索引单行读的代价可忽略，避免为一条端点在 store 层加一个与 `ListSessionsWithTitle` 部分重叠的方法。组合放在 `SessionService` 上（`GetSessionDetail` 返回 session + title），使 handler 保持"服务层取数、handler 塑形"的现有分层（与 `UpdateTitle` 一致）。

**备选**：handler 直接持有两个 store。否决 —— handler 现在不直接 import mysql store，保持这个边界。

### D3: `generating`/`run_id` —— 复用 `ActiveRuns` 单元素调用

`runStore != nil` 时以 `[sessionID]` 单元素切片调用 `ActiveRuns`；claim 读取失败降级 `generating=false`（WARN 日志），端点仍 200 —— 逐字对齐 `ListSessions` 的降级语义。`runStore == nil`（单测形态）固定 `generating=false`。

### D4: 路由分区与接线 —— api partition

- `RouteDeps` 新增 `SessionGet` 字段（handler 方法值）。
- `RegisterAPIRoutes` 在 `router.go:211-215` 的 sessions 组内注册 `authed.GET("/sessions/:session_id", deps.SessionGet)`。
- `wireAPI`（`cmd/blowball/serve.go`）接线；`agent` 角色不注册（与 `SessionList` 同命运）。
- `test/integration/role_ownership_test.go` 的 api 分区断言加入该路由。

### D5: 错误语义

- 绑定/参数：无请求体，无 400 形态。
- `GetSessionByID` 出错 → 500 `INTERNAL`（对齐 `GetSessionMessages`）。
- `sess == nil || sess.UserID != userID` → 404 `NOT_FOUND "session not found"`。
- title 读取失败 → WARN + `title` 空字符串降级（标题缺失不应打挂详情读）。

## Risks / Trade-offs

- [两个读不是快照一致（session 与 title 间有微小球差）] → 无害：两者无跨表事务约束，title 晚到最多显示旧标题/空标题一次。
- [`ActiveRuns` 失败降级 `generating=false` 可能误报"空闲"] → 与 list 行为一致；前端 attach 兜底还有 run events 端点，可接受。
- [前端误用该端点高频轮询] → 端点本身无副作用、O(1) 代价；未来如需可再加节流，不在本次范围。

## Migration Plan

纯增量路由，无部署顺序要求；回滚 = 移除路由注册。前端类型再生成（`api/openapi.yaml` → `blowball-frontend` `npm run generate-api`）。

## Open Questions

（无 —— 设计时均已定）
