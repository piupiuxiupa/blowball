# Tasks: turn-detach-resume

## 1. Redis run 状态存储层

- [x] 1.1 定义 run 状态 Redis key 族与常量：`run:{rid}:events`（Stream，MAXLEN 100000，TTL 30min）、`run:{rid}:meta`（Hash，TTL 30min）、`run:{rid}:alive`（TTL 15s）、`run:{rid}:cancel`、`session:{sid}:run`（TTL 30min）
- [x] 1.2 实现事件追加（XADD，含 done；错误交调用方 WARN，绝不阻塞）与按 cursor 读取（XRANGE 回放 + XREAD BLOCK 追 live，返回 entry id + 反序列化事件）
- [x] 1.3 实现 meta 读写与状态迁移（running → done/error/cancelled/interrupted；属主字段 user_id/session_id/model/created_at）
- [x] 1.4 实现 session 认领（SET NX 认领 / DEL 释放 / GET 读取当前 run id）与心跳（SETEX 刷新 / TTL 过期检测）
- [x] 1.5 存储层单元测试（复用现有 Redis fake/miniredis 测试模式：认领互斥、cursor 续传无重无漏、TTL/状态迁移）

## 2. Run registry（internal/run）

- [x] 2.1 实现 RunRegistry：runID → {turn cancel, done 信号}，Start/Lookup/Cancel/列表，并发安全、Close 幂等
- [x] 2.2 优雅关闭钩子：遍历全部运行中 run 逐个 cancel 并有界等待结束
- [x] 2.3 registry 单元测试（并发 cancel、重复 cancel、关闭等待）

## 3. SendMessage 解耦重构

- [x] 3.1 turn ctx 改由 registry 派生（context.Background + trace/session 元数据 + registry 持有 cancel）；HTTP request ctx 仅作用于 SSE 写出，断开连接不再传导进 agent loop
- [x] 3.2 session 认领接入：session 属主校验后 SET NX 认领，认领失败返回 409 `SESSION_BUSY`（body 带 run_id）；turn 启动失败路径释放认领
- [x] 3.3 run id 下发：`X-Run-Id` 响应头（值 = trace_id）+ 首个 `agent_start` 事件 `meta.run_id`
- [x] 3.4 事件收集 goroutine 扩展为 drainer：每事件 XADD 到 `run:{rid}:events`（含 done）、周期心跳（5s SETEX）、每 tick 轮询 cancel 标志、终局时写终态 + DEL session 认领 + 编排 60s 保留窗口后的 key 清理
- [x] 3.5 SSE 写出改为 Redis 订阅实现（发起连接与恢复端点共用一条路径）：cursor 读取逐事件写 SSE、`id:` 行 = Stream entry id、客户端断开仅关闭该订阅
- [x] 3.6 回归验证持久化路径不变：done 不入 messages 事件切片、终局（done/error/cancel）走既有 persistEvents 路径、turn_usage 照写

## 4. 取消端点

- [x] 4.1 实现 cancel handler（`POST /sessions/:sid/turns/:rid/cancel`）：meta 归属校验（不匹配 404）→ 三态分发：本进程 registry cancel / 他副本写 `run:{rid}:cancel` 标志 / 死 run（心跳过期）标记 interrupted + 强制清理 + 解锁 session
- [x] 4.2 drainer 心跳 tick 消费 cancel 标志：见标志即 cancel 本地 turn ctx 并 DEL 标志（跨副本取消延迟 ≤ 心跳周期）
- [x] 4.3 取消路径测试：本进程取消触发 partial 持久化、他副本标志路径、死 run 强制清理、他人 run 404

## 5. 恢复端点

- [x] 5.1 实现 attach handler（`GET /sessions/:sid/turns/:rid/events`，JWT + 归属校验）：status=running → 回放（或 `Last-Event-ID` 之后）+ 追 live；status 终态 → 回放后自然关流；key 不存在 → 410
- [x] 5.2 死 run 检测：追 live 期间心跳过期且 status 仍 running → 合成 interrupted `done` 事件关流 + meta 标 interrupted + 释放 session 认领
- [x] 5.3 恢复路径测试：Last-Event-ID 续传无重无漏、多订阅者并发互不干扰、终局回放关流、410 分支

## 6. 会话列表 generating 标记

- [x] 6.1 `GET /sessions` 列表项增加 `generating` 字段（按 `session:{sid}:run` 批量判定；api 角色直读 Redis）
- [x] 6.2 列表序列化测试（有/无运行中 turn 两态）

## 7. 路由与装配

- [x] 7.1 `RegisterAgentRoutes` 注册 cancel 与 turn events 两个 JWT 路由；确认 `all` 角色联合注册无 gin 重复路由
- [x] 7.2 `wireAgent` 装配：构造 run store 与 RunRegistry，注入 MessageStreamHandler 与新 handler；api 角色不构造（编译期边界不变）
- [x] 7.3 `serveRun` 优雅关闭接线：agent/all 角色退出时经 registry 有界取消全部运行中 turn

## 8. API 契约与文档

- [x] 8.1 更新 `api/openapi.yaml`：409 `SESSION_BUSY`（含 run_id）、`X-Run-Id` 头、`meta.run_id`、cancel / turn-events 端点（含 410）、sessions 列表 `generating` 字段
- [x] 8.2 更新 `CLAUDE.md`：端点表、请求流描述（解耦 + 409 + 恢复）、Redis run key 族、角色表路由变化；注明前端仓需重跑 `npm run generate-api`

## 9. 集成与回归

- [x] 9.1 `test/integration`：断开后生成继续并持久化、重开 attach 续传、运行中再发 409、取消三态、优雅关闭取消
- [x] 9.2 `make test` 与 `make lint` 全绿
