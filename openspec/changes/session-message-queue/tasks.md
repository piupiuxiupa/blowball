# Tasks: session-message-queue

实施顺序按依赖排列；每组对应一个可提交的完整特性（Conventional Commits，AGENTS.md 约定）。设计依据见 design.md（D1–D11），行为验收见 specs/。

## 1. 配置块

- [x] 1.1 `internal/config/config.go`：`MessagesConfig` 增加 `Queue` 子块（`enabled bool` 默认 false、`max_depth *int` 未设默认 8），校验显式非正 `max_depth` 拒绝加载（`*int` 区分未设与显式值，同 `max_input_tokens` 手法）
- [x] 1.2 `config.example.yaml`：补 `messages.queue` 注释示例（含回滚说明）；`internal/config` 单测覆盖默认值、显式 0/负值拒绝、缺省 byte-for-zero 行为

## 2. run Store 队列原语

- [x] 2.1 `internal/run/run.go`：定义 `QueueEntry`（run_id、content、已解析 selection 字段、附件引用、user_id、enqueued_at 的 JSON 序列化）与 `AdmitResult`（claimed / queued(position) / full）；`Store` 接口新增 `AdmitMessage`、`PumpHead`、`DequeueMessage`（按 run_id 出队并返回条目）、`QueueLen`、`QueueEntries`、`TouchQueued`（重臂 events/meta TTL）、`ClearQueue`
- [x] 2.2 `internal/run/run.go`：`StatusQueued` 常量（不进 `Terminal` 判定）；`RunKeyTTLWithJitter` 复用于 `TouchQueued`
- [x] 2.3 `internal/run/memstore.go`：全部队列原语的内存实现（claims/queue map，admit 与 pump 的原子语义用锁内单临界区模拟，供单测与集成测试）
- [x] 2.4 `internal/store/redis/runstore.go`（或新 `queuestore.go`）：Redis 实现——`admit` Lua（claim 空闲且队列为空才 SET NX 认领；否则 LLEN<max_depth 时 RPUSH 返回位置；否则返回 full）、`pump` Lua（LPOP 队头，claim 空闲则 SET NX 认领返回条目，否则 LPUSH 推回）、`DequeueMessage`（LRANGE 定位 + LREM）、`TouchQueued`/`ClearQueue`；单测覆盖 FIFO 密封（并发 admit 顺序确定、pump 认领被占推回）
- [x] 2.5 `internal/run/run_test.go`：FIFO 密封与深度上限的行为级断言（对 MemStore 与 Redis 共用语义描述）

## 3. queued 事件与状态消费方适配

- [x] 3.1 `internal/stream/event.go`：新增事件类型 `queued`（构造函数携带 `meta.run_id` 与队列位置）；确认 SSE 写出路径无需特判
- [x] 3.2 `internal/handler/turn_run.go`：`writeRunEventStream` 对 `queued` 状态的处理核验——死 run 检测仅对 `running` 触发（现状已如此，补注释与测试钉死）；queued → running 状态翻转后事件续流无重复
- [x] 3.3 `internal/handler/turn_run.go`：`CancelTurn` 增加 `queued` 分支——`DequeueMessage` 出队、`SetStatus(cancelled)`、`Retire`、按条目回滚附件 staged claim（复用 `rollbackNewAttachmentClaims`）、返回 `turnStatusBody(cancelled)`；补「取消排队消息」「取消当前轮后队列继续」「queued 不被死 run 强清误判」测试

## 4. 起轮核心抽取（纯重构，行为不变）

- [x] 4.1 `internal/handler/turn_start.go`：把 `SendMessage` 自「认领成功」到「turn goroutine + SSE 订阅等待」的段落抽成 `startTurn`（含前置段 RecoverMessages/缝合/recall、title 触发、meta 写入、send-time 用户行落库、hooks/tap/incremental persister、终局持久化 + Finalize + 接续钩子位）；入参显式携带 userMsgTime（HTTP 路径 = 请求时刻，pump 路径 = 接续时刻，D4）与已解析 selection
- [x] 4.2 `SendMessage` 改为调用 `startTurn`，队列禁用时全链路行为 byte-for-byte 不变；`internal/handler` 现有测试（message_stream/turn_run/persist/attachment/memory_stream 等）全量不改断言通过
- [x] 4.3 `statusSuffix`（`prompt.RenderStatus`）渲染点移入 `startTurn`（pump 路径取接续时环境信息）；落库内容与进上下文内容保持同一份

## 5. 入队路径（admit 失败不再 409）

- [x] 5.1 `SendMessage`：`messages.queue.enabled` 且认领被占时构造 `QueueEntry`（入队前完成全部既有校验与附件 staged claim 提交）调用 `AdmitMessage`——`claimed` → 立即 `startTurn`（快路径不变）；`queued` → `InitMeta(status=queued)` + `AppendEvent(queued 事件)` + `X-Run-Id` 头 + `writeRunEvents` 订阅（无 resultCh 等待，SSE 循环自然运行到终局）；`full` → 409 `QUEUE_FULL`；传输错误 → WARN 后直接 `startTurn`（D7 降级）
- [x] 5.2 `internal/handler` 单测：入队分支（entry 快照字段齐全、queued 事件先于 agent_start、`QUEUE_FULL`、Redis 故障降级起轮、默认关闭仍 409）
- [x] 5.3 队列等待期的用户行不落库断言：入队后 `GET /messages` 不含该消息（D4 等待期不可见）

## 6. 接续路径（pump）

- [x] 6.1 turn goroutine 终局尾部（`Finalize` 之后）：队列启用且未停机时调 `PumpHead`——弹头认领成功则以条目重建请求等价物调 `startTurn`（`SetStatus(queued→running)`、recall/title/turn-start 压缩照 D8 全套触发）；认领被占推回则结束
- [x] 6.2 `startTurn` 的 meta 写入兼容两条入口（首次 `InitMeta(running)` vs 已存在 queued meta 时 `SetStatus(running)`）；用户行 `msg_time` 取 startTurn 时刻（接续语义 D4，排序测试断言 userA < assistantA < queuedUserB）
- [x] 6.3 停机闸门：进程级「不再起轮」信号（优雅关闭序列置位），pump 前检查；停机时条目留队
- [x] 6.4 mid-turn compaction 防泄漏测试：turn A 运行中入队 B 并触发 mid-round 压缩，断言重写后的 round 不含 B（D4 坑一）
- [x] 6.5 端到端接续测试（fake orchestrator）：A 轮 done 后 B 自动起轮、B 的 SSE 流（原连接或 resume）收到 queued → agent_start → … → done、历史含 A 完整终局批次

## 7. reconciler 兜底

- [x] 7.1 `internal/handler`（或 `internal/run`）新增队列 reconciler：定时 `SCAN queue:*`（cursor 续传、每轮有界），认领空闲的会话执行 `PumpHead` 起轮（复用 6.1 路径），并对 queued run 调 `TouchQueued` 重臂 TTL；WARN-only 失败；优雅关闭退出
- [x] 7.2 `cmd/blowball/serve.go` `wireAgent`：队列启用时构造并启动 reconciler（agent/all 角色）；api 角色不构造
- [x] 7.3 测试：模拟「释放后未 pump」残留 → reconciler 周期内接续；queued run TTL 被续臂（MemStore Now 注入时间推进）

## 8. 队列读 API 与会话状态增补

- [x] 8.1 `internal/handler/queue.go` + `internal/handler/router.go`（api 分区）：`GET /api/v1/sessions/:session_id/queue`——所有权校验（404 不泄露），按入队顺序返回 `{run_id, content, model, enqueued_at, attachments}`；仅依赖 Store 读原语
- [x] 8.2 session 列表/详情：队列启用时为深度 >0 的会话 additive 携带 `queued_count`（列表用 pipeline 批量 LLEN，读取失败 WARN 降级省略字段——同 `generating` 降级惯例）
- [x] 8.3 `internal/service/session.go` `DeleteSession`：删除序列中 `ClearQueue`（排队条目不归档）；对应会话的 queued run 订阅按终局语义收流的测试

## 9. 契约与文档同步

- [x] 9.1 `api/openapi.yaml`：`queued` 事件类型、`GET /sessions/{id}/queue`、409 `QUEUE_FULL`、`queued_count` 字段、cancel 对 queued run 的语义、队列启用时 busy 场景改写入队
- [x] 9.2 `CLAUDE.md` / `AGENTS.md` 同步：路由表（queue 端点）、发送流程段落（admit/pump/持久化时机/停机/reconciler）、`messages.queue` 配置约定；`openspec` 归档前 delta 与主 specs 同步规则照旧
- [x] 9.3 提醒项（非本 repo 任务）：`blowball-frontend` 同步 openapi 并 `npm run generate-api`，实现 pending 气泡与发送解禁——记录到变更说明，不在本仓库验收

## 10. 集成与回归

- [x] 10.1 `test/integration/`：全链路排队场景——运行中连续发 B/C、按序接续、每条独立 model/effort、取消排队消息、取消当前轮后队列继续、深度上限、停机跳过接续、崩溃残留 reconciler 接续
- [x] 10.2 队列禁用回归：现有集成测试全量通过（byte-for-byte 行为保持的最终验收）
- [x] 10.3 `make lint && make test` 全绿；按组提交 Conventional Commits（feat(queue): …）

## 11. 评审修复（review round 1）

- [x] 11.1 [P1] startTurn 在发布 running 前先同步心跳（queued 订阅者的死 run 检测窗口关闭）
- [x] 11.2 [P1] DequeueMessage 用原子 LREM 计数判定真实出队（与 pump 竞争时不得误报 cancelled）
- [x] 11.3 [P2] 入队原子化：admit Lua 同步发布 entry + meta(queued) + queued 事件（消费开始后不得回写初始状态）
- [x] 11.4 [P2] reconciler 轮换处理起点（尾部会话不再饥饿，TouchQueued 全覆盖）
- [x] 11.5 [P2] fromQueue 的 meta 过期时从 QueueEntry 重建完整 RunMeta（cancel/events 归属校验不再 404）
- [x] 11.6 [P2] ClearQueue 终结被丢弃的 queued run（cancelled + retain 窗口），等待中的 SSE 收流
- [x] 11.7 回归：make test 全量 + 新增行为级测试（顺序钉死、原子入队 pin、轮换、重建、终结）

## 12. 评审修复（review round 2）

- [x] 12.1 [P1] 启动期持续心跳保活（单次 pre-publish 心跳只覆盖 15s；持久化/drainer 接手前由 keeper 持续续臂）
- [x] 12.2 [P2] ClearQueue 的快照+删除原子化（drain 后到达的条目存活走 pump 校验路径，不再被删除却未 retire）
- [x] 12.3 [P2] 取消排队消息保留其他已接受消息仍引用的附件（回滚按剩余队列条目的引用集过滤）
- [x] 12.4 回归：make test 全量 race + 三个新行为级测试（阻塞持久化期间的持续心跳、drain 语义、共享附件）

## 13. 评审修复（review round 3）

- [x] 13.1 [P1] 关停屏障覆盖已出队未注册的起轮（shutdown 派生 ctx 取消准备、pop→spawn WaitGroup 等待、中止原子返队列 RequeueHead）
- [x] 13.2 [P2] 附件引用闭包扩展（剩余队列 ∪ 已持久化行 ∪ 无心跳认领持有者的启动中守卫）
- [x] 13.3 回归：make test 全量 race + 五个新行为级测试（RequeueHead、pump 中止返队列、startTurn 检查点、持久化引用保护、启动中守卫）

## 14. 评审修复（review round 4）

- [x] 14.1 [P2] 起轮持久写顺序重排：用户行落库 → 心跳 → 发布 running（慢写期间状态保持 queued，死 run 检测本就不触发；alive 不再先于行落库出现）
- [x] 14.2 [P2] 附件守卫改为「claim holder 的行可观测」（行本位而非心跳本位；跨副本可见，顺带关闭发送时写失败的残余窗口）
- [x] 14.3 回归：make test 全量 race + 测试更新（心跳顺序钉死、慢写期间 queued 状态、持久引用/启动中守卫含 Alive=true 变体）
