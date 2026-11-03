# Design: session-message-queue

## Context

现状（`internal/handler/message_stream.go` 的 `SendMessage` + `internal/run`）：一个会话同时至多一个运行中 turn，由 Redis `SET session:{sid}:run {rid} NX`（`SessionClaimTTL` 30 分钟，心跳重臂）互斥；认领失败 → 409 `SESSION_BUSY`。turn 在脱离 HTTP 的 goroutine 里执行，事件经 `run.StartDrainer` 写入 Redis Stream `run:{rid}:events`，SSE 响应与 resume 端点是**同一个订阅循环**（`writeRunEventStream`：XRANGE 重放 + XREAD BLOCK 尾随直到终局）。turn goroutine 的终局序列为：终局消息批次持久化 → `Finalize`（终态 → 释放认领 → 60s 保留窗）。

三条既有不变量是本设计的地基：

1. **「认领空闲 ⇒ 历史完整」**（harden-turn-persistence D2）：终局批次 durable 先于认领释放。pump 出队后重建历史时，上一轮答案必然已在持久层。
2. **SSE 与 turn 解耦**（turn-detach-resume）：订阅循环天然支持「流先存在、事件后到」的长时间空尾随。
3. **run 状态机跨进程**：meta/心跳/cancel 都在 Redis，多副本共享同一信任边界。

## Goals / Non-Goals

**Goals:**

- 会话运行中仍可发送消息，进入 per-session FIFO 队列，上一轮终局后自动接续为完整的一轮。
- FIFO 严格密封：新消息不可插队，多副本/多请求并发下顺序确定。
- 排队消息的等待期对客户端可见（`queued` 事件 + 队列读 API），断开/重连/取消语义与现有 run 生命周期一致。
- 默认关闭，关闭时行为 byte-for-byte 等于现状（409 路径），回滚只动配置。

**Non-Goals:**

- 跨会话的全局并发调度/优先级（claim 仍 per-session，互斥面不变）。
- 排队消息的编辑/重排（撤回 = cancel queued；编辑不做）。
- 队列持久化到 MySQL（无 migration；Redis + AOF 是承诺等级，与 flusher 一致）。
- 客户端幂等 key（见 Open Questions）。
- 前端实现（本变更交付后端 + openapi 契约；前端在 blowball-frontend repo 消费）。

## Decisions

### D1: 队列形态 — Redis LIST `queue:{sid}`，条目为已解析请求快照

条目 JSON：`{run_id (=请求 trace_id), content, 已解析 selection (model/effort/MaxContext/MaxCompletion/LengthContinue 等), attachment 引用, user_id, enqueued_at}`。

理由：与 claim 同一存储、同一 AOF 承诺、跨副本可见；入队前完成全部校验（1MB body、token 估算、model/effort 解析、附件校验），即时 400/404 反馈，且**带解析结果入队**使 pump 副本不依赖自身配置读取时序（比现状更抗副本配置漂移）。队列 key 无 TTL（Redis 空列表自动删除 key），清理靠 pump/取消/会话删除三途。

备选（拒绝）：进程内存队列——副本间不可见、崩溃即丢；MySQL `session_queue` 表——需要 migration + 新的读写路径，Redis-first 架构下没有增量收益。

### D2: FIFO 密封 — 两个 Lua 脚本

不变量一句话：**queue 非空 ⇒ 只能 RPUSH；claim 空闲且 queue 为空 ⇒ 才允许直接认领。**

- **admit（POST 路径，单脚本原子完成）**：`if claim free AND LLEN(queue)==0 → SET NX claim={rid}（返回 claimed）`；`else if LLEN < max_depth → RPUSH + 同脚本内原子发布 meta（queued，含归属字段）与 queued 事件（返回 queued + 位置）`；`else → 返回 full`。入队原子发布（评审修复）：条目、其 meta、其 queued 事件同时可见——紧跟其后赢得竞争的 pump 永远不会观察到「迟到的初始状态」覆盖 running/done。
- **pump（终局路径，单脚本原子完成）**：`LPOP 队头 → if claim free → SET NX claim={队头 rid}（返回 entry）`；`claim 被占 → LPUSH 回队头（返回 nil）`。

密封性论证：终局释放 claim 与 pump 弹头之间即使有新 POST 插入，admit 看到队列非空（条目尚未被弹出）只能 RPUSH 排尾；pump 的弹头+认领原子完成，认领失败（仅可能来自 reconciler 竞争等）会推回队头。多 pump 并发由 claim NX 天然互斥。取消出队（DequeueMessage）以原子 LREM 的**实际删除计数**判定成功——扫描与删除之间被 pump 弹走的条目返回未出队，取消方按 run 的当前状态作答，绝不误报 cancelled（评审修复）。

### D3: POST 响应形态 — 保持 SSE 长连接，先发 `queued` 事件

入队成功后：`X-Run-Id` 头照发；事件 `queued`（新事件类型，Meta 携带 `run_id` 与队列位置）经 `AppendEvent` 写入 `run:{rid}:events`，随后 POST 响应直接走现有 `writeRunEventStream`（after="0"）——重放 `queued` 后进入空尾随，pump 起轮后事件自然续上。客户端断开无所谓（resume 端点按 run_id 重接）；每条排队消息占用一条挂起连接（HTTP/2 下无碍）。

备选（拒绝）：`202 + {run_id}` 再由客户端 attach——客户端需要「何时开始」的信号，只能轮询 session 状态，破坏「一个 POST = 一条流」的前端模型，且现有循环已免费支持长等待。

### D4: 排队消息的用户行在 pump 起轮时落库，`msg_time` 取 pump 时刻 ⚠️ 两个坑

**坑一（上下文泄漏）**：若入队时即落用户行，运行中 turn 的 mid-turn compaction 会经 `CompactionService.RecoverDurable` 重读持久层全量历史，把这条 `msg_time` 更晚的用户行缝进重写后的 round——**模型在当前轮中途看到下一条排队消息**。因此用户行必须等 pump（认领到手、orchestrator 启动前）才写，恰好落在现有 send-time persist 的位置，不变量「写在 turn 全部历史读之后」自动满足。等待期消息不出现在 `GET /messages`，由队列读 API 呈现（D10）。

**坑二（排序倒挂）**：现状用户行 `msg_time` = 请求到达时刻，且发送时批次先于本 turn 全部 assistant 行落库，排序成立。排队消息若沿用到达时刻：到达 t1 < 上一轮 assistant 行的落库时刻 t2，最终历史会把它排进上一轮答案**之前**。因此排队条目仅携带 `enqueued_at` 供展示，用户行（及 recall 注入对的 msg_time）在 pump 时取**当时刻**——pump 发生在上一轮终局批次 durable 且认领释放之后，天然大于其全部行，排序恢复「userA < assistantA < queuedUserB < assistantBB…」。

### D5: 接续驱动 — turn goroutine 尾部 pump + reconciler 兜底 + 停机闸门

- **正常路径**：turn goroutine 在 `Finalize` 之后同步调 `pumpQueue(sid)`：pump Lua 弹头认领，成功则调用与 HTTP 路径共享的「起轮核心」（D8），后者再起自己的 turn goroutine——轮与轮是 goroutine 链，不是栈递归。起轮的持久写顺序（评审修复第 4 轮定稿）：**meta（queued）→ 发送时用户行落库 → 同步心跳 → 发布 running**。排队 POST 的 SSE 订阅者已在尾随——慢写期间状态保持 queued（死 run 检测只对 running 触发，无需心跳，订阅者继续等待，与排队期行为一致）；行落库后的同步心跳 + 启动期 keeper（复用 startPersistHeartbeat，幂等 stop）保证 running 可见时 alive 必未过期，且 **alive 的出现不早于行落库**——心跳证明进程活着，不证明行已持久，附件引用守卫据此以「行可观测」为本位而非心跳。
- **崩溃残留**：进程死在「释放认领后、pump 前」，队列干等。agent 角色起一个 **reconciler** goroutine（模式照抄 `internal/msgflush`）：定时 `SCAN queue:*`（cursor 续传、每轮有界且**轮换处理起点**——评审修复：稳定的扫描顺序加上每轮上限会让尾部会话永久饥饿，轮换保证积压超过单轮上限时尾部同样获得 pump 机会与 TTL 续臂），对每个 sid 尝试 pump；顺带为 queued 状态 run 重臂 `run:{rid}:events`/`meta` TTL（`RunKeyTTL` 才 1h，深队列/慢轮会等到过期——SSE 订阅循环对 meta 消失的契约是收流，reconciler 使其罕见）。多副本安全：pump Lua 的 claim NX 互斥。若等待期 meta 已过期（reconciler 亦不可用），起轮时从 QueueEntry 重建**完整** RunMeta（含归属与 model 字段——仅写 status 的部分重建会让 cancel/events 的归属校验 404，评审修复）。
- **停机闸门（评审修复三轮）**：优雅关闭会取消运行中 turn，取消路径也走 Finalize——若不拦，关机时 pump 会把队头**顶起来跑**。闸门为通道 + shutdown 派生 context：pump 前检查（停机后不再弹出新条目）；已弹出条目的启动阶段（会话查找→历史恢复→发送时持久化，至多 60s）挂在该 context 上——停机即取消；startTurn 在 fromQueue 路径的**首个持久写之前**设检查点，中止时以 RequeueHead（LPUSH 回队头 + 自己的认领比较删除，单脚本原子）返还条目，重启后 reconciler 原样接续。pop→spawn 期间以 WaitGroup 跟踪（Register 发生在持久化之后，CancelAll/WaitAll 看不到这段窗口），serveRun 关停序列在闸门后等待其归零——既不在「主流程结束后」再启动任务，也不在进程退出时丢失已弹出的消息。

取消排队消息的附件回滚按**完整引用闭包**过滤（评审修复二至四轮）：同会话复用合法——引用本条目首次认领附件的消息可能仍在队列、已被弹出启动、甚至已完成持久化。回滚仅作用于「无任何引用」的 id，引用集 = 剩余队列条目 ∪ 会话已持久化行（水合附件引用，覆盖已弹出/运行中/已完成，**经 ListDurableMessages 直读权威 MySQL 层**——读缓存是 best-effort 加速层，命中可能滞后，不能用于证明引用缺席）∪ **持有者用户行守卫**（会话认领的持有者的 RoleUser 用户行在持久层不可见 = 弹出未落库的 preamble、发送时写失败运行中（行要到轮终才落库）、或已死持有者——引用不可观测时整体挂起回滚；匹配必须是用户行本身：memory-recall 的 tool-call 占位行与用户行同 TraceID 且先落库，任意行匹配会假阳性）。守卫以**行可观测性**为准而非心跳：心跳在起轮的一个阶段先于行写、另一阶段后于行写，且跨副本下行共享而心跳不共享。任一读取失败跳过回滚（fail-safe：孤儿绑定附件随会话删除清理，优于破坏存活消息）。

### D6: 新 run 状态 `queued` + 取消分支

`run:{rid}:meta.status` 增加常量 `queued`（入队时 `InitMeta` 置入；pump 起轮时置 `running`；不参与 `Terminal` 判定）。消费方适配：

- **`writeRunEventStream`**：queued ≠ running，不触发死 run 检测（无 alive key），自然空尾随；取消置 cancelled 后按终局收流。
- **`CancelTurn`**：现状对「非 terminal、registry 未命中、无心跳」的 run 走 **dead-run force-clear → interrupted**——排队 run 恰好长这样，会被误判。新增 queued 分支：`LREM` 出队（按 run_id 定位条目）+ `SetStatus(cancelled)` + `Retire`；条目携带的附件 staged claim 逐一回滚（复用 `rollbackNewAttachmentClaims`）。取消运行中轮后队列照常接续（用户想全停则逐条取消或删会话）。
- **`TurnEvents`**：meta 存在且归属匹配即可订阅（现状已如此），queued 状态天然可尾随。

### D7: Redis 故障降级 — 直接起轮

admit Lua 不可执行（Redis 不可达）时：无法入队（队列本身就在 Redis），沿用现状「认领传输失败 → 无锁放行」惯例，WARN 后直接起轮。「messages always win」优先于互斥与排队，byte-for-byte 平移现有约定。

### D8: 「起轮核心」抽取 — HTTP 路径与 pump 路径共用

`SendMessage` 从「认领成功」到「turn goroutine + SSE 订阅」的部分抽成 `startTurn`（建议 `internal/handler/turn_start.go`）：前置段（RecoverMessages → 持久层缝合 → recall → 组装 agentMsgs）→ title 触发 → `InitMeta`/`SetStatus(running)` → send-time 用户行落库（D4）→ hooks/tap/incremental persister → turn goroutine（终局持久化 → Finalize → **pump 下一条**）。两条入口：

- HTTP 快路径：admit 返回 claimed → 立即 `startTurn`（行为与现状 byte-identical）。
- pump 路径：pump Lua 返回 entry → 用条目快照重建请求等价物 → `startTurn`。recall/title 均在此时触发（此时上一轮答案已在历史，recall 查询质量更高）；title cadence 的用户序数照算——「409 从不触发」平移为「排队在 pump 时触发」。

HTTP 侧保留的职责：请求绑定与全部入队前校验、附件 staged claim 提交（入队即提交，防重复引用；取消时按条目回滚）、admit 调用、SSE 响应写出。`statusSuffix`（prompt.RenderStatus）移到起轮时渲染（pump 路径拿到更新鲜的环境信息；落库内容与进上下文内容保持同一份）。

### D9: 配置 — `messages.queue` 默认关

```yaml
messages:
  queue:
    enabled: false   # off = 409 行为 byte-for-byte 不变
    max_depth: 8     # 满时 409 QUEUE_FULL；正整数，非正拒绝加载
```

落在现有 `messages:` 块（flush/input-token 同居），校验规则沿用（负/零 max_depth 拒绝）。开启后 POST 不再因 busy 返回 409（仅 QUEUE_FULL）。

### D10: 队列读 API — `GET /api/v1/sessions/:session_id/queue`（api 分区）

返回条目数组（`run_id`、`content`、`model`、`enqueued_at`、附件引用），供前端刷新后恢复 pending 气泡。归 api 分区（只读 LIST，仅需 run Store 的读原语，与「api 角色构造 Store 不构造 Registry」的既有边界一致）。`Session list`/`Session detail` additive 增补 `queued_count`（>0 时携带），侧栏徽标用；判据为 `LLEN queue:{sid}`。

### D11: 会话删除清空队列

`DeleteSession` 在现有「drain → archive+purge → 清缓存」序列中增加清队列（数据库优先、缓存其次的既有次序内）：`ClearQueue` 以**单脚本原子快照+删除** LIST 并将每个被丢弃的 queued run 置为 `cancelled` + 保留窗 retire（评审修复两轮：仅删 LIST 会让等待中的 SSE 悬挂到 TTL；快照与删除分离则会让竞态入队的条目被删却未 retire——原子 drain 保证 retire 的恰是被删除的集合，drain 后到达的条目存活走 pump 的会话校验路径）。排队条目不是消息行（尚未落库），无归档镜像义务——删除即弃。

## Risks / Trade-offs

- [违反 D4 时 mid-turn compaction 泄漏排队消息进运行中 turn] → spec 以场景钉死「运行中 turn 的 durable 恢复不包含排队消息」；集成测试：入队 → 触发 mid-turn 压缩 → 断言 round 上下文无排队内容。
- [排队消息 msg_time 倒挂] → D4 取 pump 时刻；测试断言历史顺序 userA < assistantA < queuedUserB。
- [admit/pump 并发窗口破 FIFO] → D2 单脚本原子；并发 admit 压测（多 goroutine 同时入队，断言队内顺序 = 完成顺序的线性化子序列）。
- [CancelTurn 把 queued run 误判为死 run] → D6 显式分支；测试：入队 → cancel → 状态 cancelled、出队、SSE 收流、队列后续接续不受影响。
- [停机时 pump 顶起新轮] → D5 闸门；优雅关闭集成测试断言停机后无新 turn 启动、条目仍在队列。
- [等待期 run-key TTL 过期（reconciler 亦不可用）] → 契约降级：SSE 按既有「key 消失即收流」结束，条目仍在队列，pump 时 `InitMeta` 重建 meta、照常起轮，客户端可重放。罕见路径，接受。
- [客户端网络重试双入队（无幂等 key）] → v1 接受（现状由 409 无意间挡住，本变更移除该副作用）；见 Open Questions。
- [reconciler 为已离开的用户续跑排队消息（付费 LLM 调用）] → 与 turn-detach-resume「断开不取消」哲学一致（用户已接受该消息）；深度上限（默认 8）约束最大爆炸半径；文档化取舍，不加「队列过期」。
- [`startTurn` 抽取引入回归] → 抽取后现有 message_stream 全量测试不改断言必须全绿；两条入口各补一条端到端集成测试。

## Migration Plan

1. 部署含本变更的二进制（`messages.queue.enabled` 缺省/显式 false——行为不变，可先行灰度二进制）。
2. 开启：config 增 `messages.queue: {enabled: true, max_depth: 8}`，重启 agent 角色进程（api 角色无需重启，队列读 API 随新二进制生效）。
3. 契约：`api/openapi.yaml` 变更同步 `blowball-frontend`（`npm run generate-api`）；前端按 `queued` 事件 + queue 读 API 实现 pending 气泡与发送解禁。
4. 无 DB migration；Redis 前置仍是 AOF（既有部署前提，排队条目与 flush 窗口同一承诺等级）。
5. **回滚**：停 agent 进程（运行中轮走优雅关闭持久化）→ 等队列排空或确认空 → 关开关重启。已存在的孤儿条目在开关关闭后 inert（无 pump 消费方），可 `DEL queue:{sid}` 手工清理；queue 读 API 在开关关闭时报空。

## Open Questions

- 客户端幂等：是否引入可选 `client_message_id`（入队去重 + 重试安全）？v1 不做，待真实重发事故推动。
- `done.meta.next_run_id` 糖（终局事件携带下一条 run id，前端免查队列）——done 发射先于 pump，需在 done 时刻读队头且可能失真，v1 不做。
- `max_depth` 默认 8 是否合适（真实使用反馈后调整；上限只挡入队速率，不挡 LLM 成本——成本由 reconciler 跑完整个队列的取舍约束）。
