## Why

当前系统的 `user_id`、`session_id`、`trace_id` 使用 UUID v4 或客户端传入，无法利用 UUID v7 的时间有序性改善数据库索引与日志排序；同时 `session_id` 由客户端生成并由 `EnsureSession` 隐式创建，导致会话生命周期不清晰。通过服务端统一生成 UUID v7 并显式暴露 `POST /sessions` 创建接口，可以提升 ID 可排序性、明确会话创建边界，并为前端提供可靠的历史消息恢复能力。

## What Changes

- **UUID v7 统一化**
  - `trace_id`：中间件 `trace.New()` 改为 `uuid.NewV7().String()`。
  - `user_id`：种子脚本 `cmd/seed/main.go` 改为 `uuid.NewV7().String()`（当前无注册接口）。
  - `session_id`：新增 `SessionService.CreateSession(userID)`，内部使用 `uuid.NewV7().String()` 生成。
- **新增接口**
  - `POST /api/v1/sessions`：鉴权后创建新会话，返回 `{"session_id": "..."}`。
  - `GET /api/v1/sessions/:session_id/messages`：鉴权后分页返回该会话完整事件流，按 `(msg_time, msg_index)` 排序，并校验用户所有权。
- **修改现有接口行为（BREAKING）**
  - `POST /api/v1/sessions/:session_id/messages` 不再自动创建会话；session 不存在或归属其他用户时返回 404。
- **依赖不变**
  - 继续使用 `github.com/google/uuid v1.6.0`（已支持 UUID v7），不引入新依赖。

## Capabilities

### New Capabilities
- `session-crud`：显式创建会话以及按会话读取历史消息的能力。

### Modified Capabilities
- `session-management`：会话创建方式从"客户端生成 session_id 并在首次发消息时自动创建"改为"服务端生成 session_id，客户端需先调用 `POST /sessions` 创建"。
- `api-server`：新增两条需要鉴权的路由 `POST /api/v1/sessions` 和 `GET /api/v1/sessions/:session_id/messages`，并更新 `POST /api/v1/sessions/:session_id/messages` 的行为描述。

## Impact

- `internal/pkg/trace/trace.go`：修改 `New()` 生成逻辑。
- `cmd/seed/main.go`：修改 user_id 生成逻辑。
- `internal/service/session.go`：新增 `CreateSession`，修改 `EnsureSession`（或移除/替换为严格检查）。
- `internal/service/deps.go`：可能调整 `MySQLStore` / `FSStore` 接口。
- `internal/handler/session.go`：新增 `CreateSession` 和 `GetSessionMessages` handler，修改 `SendMessage` 的 session 存在性校验。
- `internal/handler/router.go`：新增路由依赖与注册。
- `internal/store/mysql/session.go` / `message.go`：可能需要新增按 session 分页查询方法。
- 测试：handler/service/integration 测试需要同步更新 mock 与断言。
