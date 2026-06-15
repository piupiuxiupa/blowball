## 1. UUID v7 基础改造

- [x] 1.1 修改 `internal/pkg/trace/trace.go`，`New()` 使用 `uuid.NewV7().String()` 生成 trace_id，更新包注释与单元测试。
- [x] 1.2 修改 `cmd/seed/main.go`，user_id 生成改为 `uuid.NewV7().String()`，确保种子用户可正常创建。

## 2. Session 服务层新增与改造

- [x] 2.1 在 `internal/service/session.go` 新增 `CreateSession(ctx, userID) (string, error)`：生成 uuid7 session_id，调用 `fs.EnsureUserDirs`，写入 MySQL `sessions` 表。
- [x] 2.2 修改 `internal/service/session.go` 中现有方法，确保 `SendMessage` 不再调用 `EnsureSession`；新增/复用 session 存在性与所有权校验逻辑。
- [x] 2.3 更新 `internal/service/deps.go` 中的 `MySQLStore` / `FSStore` 接口签名（如需要新增 `ListMessagesPaged` 或类似方法）。
- [x] 2.4 更新 `internal/service/session_test.go` 与 fakes，覆盖 `CreateSession` 与严格模式校验。

## 3. MySQL 存储层扩展

- [x] 3.1 在 `internal/store/mysql/session.go` 确认 `GetSessionByID` 已可用于所有权校验；如不足则补充按 `(session_id, user_id)` 查询。
- [x] 3.2 在 `internal/store/mysql/message.go` 新增 `ListMessagesPaged(ctx, sessionID, cursor, pageSize, order) ([]model.Message, nextCursor, error)`，支持按 `(msg_time, msg_index, id)` 游标分页。
- [x] 3.3 添加/更新 `internal/store/mysql` 单元测试，验证分页边界与排序。

## 4. Handler 与路由

- [x] 4.1 在 `internal/handler/session.go` 新增 `CreateSession` handler，返回 `{"session_id": "..."}`。
- [x] 4.2 在 `internal/handler/session.go` 新增 `GetSessionMessages` handler，解析 `page_token`、`page_size`、`order`，校验所有权，返回分页消息列表。
- [x] 4.3 修改 `internal/handler/session.go` 的 `SendMessage`：删除 `EnsureSession`，改为 `GetSessionByID` + 用户所有权校验，不存在/越权返回 404。
- [x] 4.4 更新 `internal/handler/router.go` 的 `RouteDeps`，新增 `CreateSession` 与 `GetSessionMessages` 字段，并注册到 `/api/v1` 鉴权组。
- [x] 4.5 更新 `cmd/server/main.go` 中 `RouteDeps` 的构造，传入新 handler。
- [x] 4.6 更新 `internal/handler/session_test.go`、integration tests 与 harness，确保路由与行为正确。

## 5. 游标分页工具

- [x] 5.1 新增或复用游标编解码工具（建议在 `internal/pkg/cursor` 或 `internal/handler` 内部实现），将 `(msg_time, msg_index, id)` 编码为安全的 base64 字符串。
- [x] 5.2 在 `GetSessionMessages` 中集成游标解析与生成，处理空游标（首页）与末页（无 next_page_token）场景。

## 6. 集成与回归

- [x] 6.1 运行 `go test ./...`，修复因接口变更导致的编译与测试失败。
- [x] 6.2 运行 integration tests，验证 SSE 消息流、会话创建、历史消息分页完整链路。
- [x] 6.3 手动或脚本验证：
  - [x] `POST /api/v1/sessions` 返回 uuid7 session_id；
  - [x] `GET /api/v1/sessions/:id/messages` 分页返回完整事件流；
  - [x] 旧客户端不带 session_id 直接发消息返回 404。
- [x] 6.4 更新相关注释、包文档与错误信息，保持与 spec 一致。

## 7. 收尾

- [x] 7.1 运行 `go vet ./...` 与 `gofmt` 检查。
- [ ] 7.2 执行 `/opsx:archive` 归档变更（实现完成后）。
