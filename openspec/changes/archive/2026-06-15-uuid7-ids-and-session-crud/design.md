## Context

当前系统：
- `trace_id` 由 `internal/pkg/trace/trace.go` 的 `New()` 使用 `uuid.NewString()`（UUID v4）生成，中间件为每个请求附加。
- `user_id` 仅在 `cmd/seed/main.go` 通过 `uuid.NewString()` 生成并写入 `users` 表；系统没有注册接口，登录时只读取已有用户。
- `session_id` 由客户端生成并通过 `POST /api/v1/sessions/:session_id/messages` 传入；handler 调用 `SessionService.EnsureSession` 在首次发消息时隐式创建会话。
- 历史消息目前没有只读恢复接口，仅在 `SendMessage` 内部通过 `MessageService.RecoverMessages` 拉取上下文。

本变更要在不引入新依赖的前提下，把三类 ID 统一为 UUID v7，并将会话创建从"隐式/客户端生成"改为"显式/服务端生成"，同时暴露分页历史消息接口。

## Goals / Non-Goals

**Goals：**
- `trace_id`、`user_id`、`session_id` 统一使用 UUID v7 生成。
- 新增 `POST /api/v1/sessions`，服务端生成并返回 `session_id`。
- 新增 `GET /api/v1/sessions/:session_id/messages`，支持分页、用户鉴权、返回完整事件流。
- `POST /api/v1/sessions/:session_id/messages` 改为严格模式：session 不存在或不属于当前用户时返回 404。
- 复用现有 `model.Message` 数据模型与三层存储基础设施，避免数据迁移。

**Non-Goals：**
- 不新增用户注册接口（仅修改 seed 脚本中的 user_id 生成方式）。
- 不改消息三层存储的写入策略（仍按现有 Redis → FS → MySQL 逻辑）。
- 不改 SSE 事件格式与 orchestrator 事件收集逻辑。
- 不对消息内容做再加工或合并（保持原始事件粒度）。

## Decisions

### 1. UUID v7 生成统一放在一处
- `trace_id`：继续由 `trace.New()` 生成，内部改为 `uuid.NewV7().String()`。所有使用方（`TraceMiddleware`、`AuthMiddleware` 回退）无感知。
- `user_id`：种子脚本直接调用 `uuid.NewV7().String()`。
- `session_id`：由 `SessionService.CreateSession` 生成，避免 handler 直接依赖 uuid 包，保持业务逻辑内聚。
- **理由**：UUID v7 时间有序，可改善 `sessions` 表按 `update_time`/`create_time` 的索引局部性，也便于日志按 trace_id 快速排序；`google/uuid` 已支持，无需新依赖。

### 2. 严格模式处理 `POST /sessions/:id/messages`
- `SendMessage` 不再调用 `EnsureSession`，改为先 `GetSessionByID(sessionID)`，再校验 `session.UserID == userID`。
- 不存在或 user_id 不匹配统一返回 404，避免越权探测。
- **理由**：既然新增显式创建接口，就不应再允许随机 URL 隐式创建会话；统一 404 是更安全的失败模式。

### 3. `CreateSession` 职责划分
- Handler `CreateSession`：解析请求、取 user_id、调用 service、返回 JSON。
- `SessionService.CreateSession`：
  1. 调用 `fs.EnsureUserDirs(ctx, userID)` 创建用户目录。
  2. 生成 uuid7 `session_id`。
  3. 构造 `model.Session{SessionID, UserID, TraceID}` 并调用 `mysql.CreateSession`。
- **理由**：保持 handler 薄、service 拥有业务规则、store 只负责持久化。

### 4. 历史消息接口以 MySQL 为权威源
- `GET /sessions/:id/messages` 直接查询 MySQL messages 表，按 `(msg_time, msg_index)` 排序并分页。
- 不走 `MessageService.RecoverMessages` 的三层回退链，因为：
  - 该接口目标是"恢复历史消息"，MySQL 是最终持久化源；
  - 三层回退链会把 Redis/FS 数据回填，与分页语义耦合且难以保证稳定游标；
  - SSE 写入流程已保证用户消息同步、assistant 事件 goroutine 异步但最终会落库。
- **理由**：简化分页实现，避免 Redis/FS 与 MySQL 在边界情况下的不一致影响接口行为。

### 5. 分页采用 cursor 方案
- 使用 `page_token`（游标）+ `page_size`（默认 50，最大 200）。
- 游标编码最后一条消息的 `(msg_time, msg_index, id)`，避免深分页偏移抖动。
- 首次请求可不传 `page_token`。
- **替代方案 offset/limit**：实现简单，但大 session 深分页性能差，且并发写入时重复/遗漏风险更高。cursor 更适合事件流追加写场景。
- **理由**：消息表是追加写、按时间有序，cursor 分页更稳定。

### 6. 默认排序与 `order` 参数
- 默认 `order=asc`，按 `(msg_time, msg_index)` 升序，符合客户端从上到下渲染对话历史的习惯。
- 支持 `order=desc` 用于倒序浏览（如从最新开始）。
- **理由**：默认升序与现有 `RecoverMessages` 的 `(msg_time, msg_index) ASC` 一致，减少前端适配成本。

### 7. 返回完整事件流
- 接口返回 `[]model.Message` 风格的 JSON，包含所有 `event_type`（`message`、`token`、`tool_call`、`agent_start`、`agent_end`、`agent_error`）。
- **理由**：与 `session-management` 现有"Recovery returns full event stream"要求一致，前端可据此重建完整交互过程。

## Risks / Trade-offs

- **[Risk] UUID v7 暴露时间信息，可能被推断请求时序。**
  → **Mitigation**：`trace_id` 仅用于内部日志与响应头 `X-Trace-Id`，不对外作为安全令牌；`session_id` 与 `user_id` 都需要鉴权才能访问，时间可预测性不直接构成攻击面。若未来有更高匿名性需求，可再切回 v4 或加密随机 ID。

- **[Risk] `SendMessage` 改为严格模式后，旧客户端（未先调用 POST /sessions）会收到 404。**
  → **Mitigation**：这是设计上的 BREAKING 变更，需在变更说明与部署文档中明确；同时新增 `POST /sessions` 提供替代路径。

- **[Risk] GET messages 直接读 MySQL，若 assistant 事件 goroutine 尚未落库，可能看不到最新一条回复。**
  → **Mitigation**：该接口用于"恢复历史消息"，通常在用户重新进入会话时调用，此时 goroutine 已完成写入；若需强一致读取最新消息，可后续增加查询后等待或走 RecoverMessages 的同步路径，但不在本变更范围内。

- **[Risk] cursor 分页需要稳定的排序键。**
  → **Mitigation**：游标使用 `(msg_time, msg_index, id)` 三元组；`id` 是自增主键，可保证唯一性，避免同毫秒同 msg_index 的边界歧义。

## Migration Plan

- **数据库**：无需迁移。`users.user_id`、`sessions.session_id`、`sessions.trace_id`、`messages.trace_id` 均为 `CHAR(36)`，UUID v7 字符串长度相同。
- **代码**：按 tasks.md 顺序修改后，运行单元测试与 integration tests。
- **客户端**：需要更新为先调用 `POST /sessions` 获取 `session_id`，再发消息。旧行为将返回 404。
- **回滚**：若需回滚，可回退代码并保留已有 UUID v7 数据；UUID v7 与 v4 在字符串格式上兼容，混合存在不影响业务逻辑。

## Open Questions

- `page_size` 默认 50 / 最大 200 是否满足前端需求？
- `GET /sessions/:id/messages` 是否需要支持只过滤特定 `event_type`（例如仅看 `message`）？
- 是否需要在 `POST /sessions` 响应中返回 `create_time` 或预设标题字段？
