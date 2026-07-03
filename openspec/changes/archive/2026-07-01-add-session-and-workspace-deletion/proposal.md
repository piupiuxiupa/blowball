## Why

当前系统只能创建会话、追加消息、上传/下载/读取工作空间文件，**没有任何删除接口**，用户无法清理自己的数据。直接物理删除会丢失数据、无法审计或恢复；而 MySQL 虽有 `ON DELETE CASCADE`（sessions → titles → messages），级联删除是不可逆的硬删除——因此删除前必须先把数据“原样”归档保留。

侦察确认：现有删除原语零散未接通（`fs.DeleteSession` 已存在但 MySQL 无任何 delete 方法；`redis.ClearMessages`/`DelSessionCache` 已存在但未在 service 接口暴露）；所有读路径都先过 `GetSessionByID` 所有权校验，硬删除后读请求直接 404，**Redis 留待 TTL 自然过期在当前代码下已被验证安全**；工作空间文件只存在于文件系统、MySQL 无任何文件表。

## What Changes

- **新增 `DELETE /api/v1/sessions/:session_id`**：鉴权 + 所有权校验 → 在单个 MySQL 事务内把 `sessions`/`titles`/`messages` 行原样写入对应 `*_deleted` 镜像表 → 删除 `sessions` 行（级联清除 live 的 titles/messages）→ 删除 FS 会话 JSON 文件。**Redis 不处理**（TTL 自然过期）。返回 204。
- **新增 `DELETE /api/v1/workspace/files/*path`**：鉴权 + `xizhi.ValidatePath` 路径校验 → `os.RemoveAll` **递归**删除文件或目录 → 204。文件无 DB 源表，**不写归档**。
- **新增迁移 `migrations/008_deletion_archive.sql`**：为四张现有表各建一张镜像表 `users_deleted` / `sessions_deleted` / `titles_deleted` / `messages_deleted`，逐列原样保留源表所有列 + `deleted_at` + `deletion_id` 审计列，**无外键**，`messages_deleted.id` 为普通 `BIGINT`（保留源 id，非 AUTO_INCREMENT）。
- **MySQL store**：新增 `DeleteSession(ctx, sessionID)`，事务内完成 `INSERT…SELECT ×3` + `DELETE FROM sessions`；扩展 `MySQLStore` 接口。
- **Service / Handler / Router / main.go**：新增 `SessionService.DeleteSession`、`SessionHandler.DeleteSession`、`WorkspaceHandler.Delete`；`RouteDeps` 增加两个字段并注册两条 DELETE 路由；main.go 装配。
- **更新 `api/openapi.yaml`**：补充两个 DELETE 接口（含 401/403/404/204）。
- **仅后端**：不执行 `npm run generate-api`、不改前端。
- **顺手修正**：`CLAUDE.md` 中关于 `007_doris_schema.sql` 的描述已失效（Doris 被永久放弃、文件已删）。

## Capabilities

### New Capabilities

- `deletion-archive`：定义 `*_deleted` 镜像表结构与“删除前原样归档 + 原子清除”行为，以及 `users_deleted` 作为脚手架预留。

### Modified Capabilities

- `session-crud`：新增“删除会话”需求（归档 → 清除 → 清理 FS，所有权校验返回 404，Redis 不处理）。
- `workspace-api`：新增“删除工作空间文件或目录”需求（递归 `os.RemoveAll`，路径越界 403，不存在 404，不写归档）。
- `api-server`：鉴权路由组新增两条 DELETE 路由。

## Impact

- **新增迁移**：`migrations/008_deletion_archive.sql`（4 张镜像表，单文件）。
- **修改 store**：`internal/store/mysql/session.go`（或新增 `archive.go`）新增 `DeleteSession` 事务方法 + 镜像表 SQL；`internal/service/deps.go` 的 `MySQLStore` 接口新增 `DeleteSession`。
- **修改 service**：`internal/service/session.go` 新增 `SessionService.DeleteSession(ctx, userID, sessionID)`（所有权校验 → mysql.DeleteSession → fs.DeleteSession best-effort）。
- **修改 handler**：`internal/handler/session.go` 新增 `DeleteSession`；`internal/handler/workspace.go` 新增 `Delete`。
- **修改 router / main**：`internal/handler/router.go` `RouteDeps` 增加 `SessionDelete` / `WorkspaceDelete` 并注册 DELETE 路由；`cmd/server/main.go` 装配。
- **测试**：更新 6 个测试 fake（`internal/service/fakes_test.go`、`internal/handler/session_test.go`、`test/integration/harness_test.go`，约 5 个 stub）+ 新增 store/handler 单测与集成测试。
- **文档**：`api/openapi.yaml` 两个 DELETE 接口；修正 `CLAUDE.md:155`。
