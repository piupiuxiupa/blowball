## 1. 数据库迁移

- [x] 1.1 新增 `migrations/008_deletion_archive.sql`，创建 `users_deleted` / `sessions_deleted` / `titles_deleted` / `messages_deleted`：源列原样 + `deleted_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP` + `deletion_id CHAR(36) NOT NULL`；无外键；`messages_deleted.id` 为普通 `BIGINT`（保留源 id）；按 `user_id` / `session_id` 建索引；遵循 InnoDB + utf8mb4 + 注释风格
- [x] 1.2 本地 `docker compose up -d`（干净卷）验证 4 表按字母序创建成功

## 2. MySQL Store

- [x] 2.1 `internal/store/mysql/`（`session.go` 或新增 `archive.go`）新增 `DeleteSession(ctx, sessionID) error`：`BEGIN` → mint `deletion_id`(UUID) → `INSERT sessions_deleted SELECT s.*, ?, NOW()` → `INSERT titles_deleted SELECT t.*, ?, NOW()` → `INSERT messages_deleted SELECT m.*, ?, NOW()` → `DELETE FROM sessions WHERE session_id=?`（级联清 live titles/messages）→ `COMMIT`
- [x] 2.2 会话行不存在时返回 nil（幂等，匹配仓库 not-found 约定，不产生归档）
- [x] 2.3 任一 SQL 失败 → `ROLLBACK` 并返回错误
- [x] 2.4 单测（`internal/store/mysql/session_test.go`，沿用 `MYSQL_TEST_DSN` 跳过模式）：归档完整性、级联清除、幂等、事务回滚后 live 数据完好

## 3. Service 层

- [x] 3.1 `internal/service/deps.go` 的 `MySQLStore` 接口新增 `DeleteSession(ctx, sessionID) error`
- [x] 3.2 `internal/service/session.go` 新增 `SessionService.DeleteSession(ctx, userID, sessionID) error`：`GetSessionByID` 所有权校验（不存在/不匹配 → 返回可辨识 not-found）→ `mysql.DeleteSession` → `fs.DeleteSession`（best-effort，记日志不阻断）
- [x] 3.3 更新 `internal/service/fakes_test.go`：`fakeMySQLStore` +`DeleteSession`（`fakeFSStore` 已有）
- [x] 3.4 单测：三层编排成功、所有权 not-found、FS 失败 best-effort 不影响 MySQL 结果

## 4. Handler 与路由

- [x] 4.1 `internal/handler/session.go` 新增 `DeleteSession(c)`：所有权校验 → 404；`service.DeleteSession` → 204；内部错 → 500
- [x] 4.2 `internal/handler/workspace.go` 新增 `Delete(c)`：`xizhi.ValidatePath` → 403；`os.Stat` 不存在 → 404；`os.RemoveAll` → 204；错 → 500
- [x] 4.3 `internal/handler/router.go`：`RouteDeps` 增加 `SessionDelete` / `WorkspaceDelete` 字段；注册 `authed.DELETE("/sessions/:session_id", deps.SessionDelete)` 与 `authed.DELETE("/workspace/files/*path", deps.WorkspaceDelete)`
- [x] 4.4 `cmd/server/main.go` 装配两个新 handler 字段
- [x] 4.5 `internal/handler/session_test.go`：更新 `handlerFakeMySQL` +`DeleteSession`；新增 handler 单测（会话 204/404/500、文件 204/403/404）
- [x] 4.6 `test/integration/`：`memoryMySQL` +`DeleteSession`；3 处 `RouteDeps` 注册 `SessionDelete`；新增端到端用例（DELETE 后 sessions/titles/messages 行清除、FS JSON 清除、归档落 `*_deleted`、Redis 忽略、再次 GET 返回 404）

## 5. 接口文档与收尾

- [x] 5.1 `api/openapi.yaml` 新增 `DELETE /api/v1/sessions/{session_id}`（401/404/204）与 `DELETE /api/v1/workspace/files/{path}`（401/403/404/204）
- [x] 5.2 修正 `CLAUDE.md` 关于 `007_doris_schema.sql` 的失效描述（Doris 已永久移除）
- [x] 5.3 `make test` 与 `make lint` 全绿
- [x] 5.4 确认**未**执行 `npm run generate-api`、**未**改动前端（后端 only）
