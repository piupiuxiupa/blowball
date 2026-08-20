# Design: turn-detach-resume

## Context

当前 `MessageStreamHandler.SendMessage`（`internal/handler/message_stream.go`）把 orchestrator goroutine 绑定在 `c.Request.Context()` 上：SSE 连接断开 = ctx cancel = agent loop 被杀，随后走 partial persistence。`stream.Hub` 是单消费者 buffered channel，`WriteSSE` 是唯一消费者，事件没有可重放副本。取消的唯一途径就是断开连接；没有按 id 取消、也没有重连续传的机制。

本设计将 turn 生命周期与 HTTP 连接解耦：turn 由服务端 run registry 持有，事件经 Redis Stream 落日志，任意 SSE 连接（发起连接或恢复连接）都只是该日志的一个订阅者。Redis 已是部署前提（AOF），api/agent 角色拆分与 shared data plane 下跨副本一致性也由 Redis 承担。

## Goals / Non-Goals

**Goals:**
- 断开 SSE 不取消生成；turn 一律跑到 done / error / 显式取消
- 每请求一个 run id（context id）：随响应下发、可按 id 取消并释放资源
- 重开会话时对未完成 turn 自动恢复续传（重放 + 追 live）
- 运行中 session 拒绝新消息（409 `SESSION_BUSY`），互斥跨进程成立
- 中间状态全部落在 Redis（事件日志 / run 元数据 / session 认领）
- 复用现有 partial-turn 持久化路径，`done` 事件不进 `messages`（现状不变）

**Non-Goals:**
- 不做跨进程 turn 迁移（进程死 = turn 中断，不在别的副本续跑）
- 不做新消息排队；运行中就是 409
- 不推广 mid-turn flush（崩溃窗口的 messages 持久化缺口维持现状，崩溃靠 run 日志回放 partial）
- 不改 Hub 的单消费者语义；不改 SSE 事件格式与 `done.usage` 结构
- 不引入新的 MySQL schema（run 状态只活在中断可重建的 Redis 里）

## Decisions

### D1: turn ctx 由进程内 run registry 持有，run id 复用 trace_id

新建 `internal/run`（进程内 `RunRegistry`）：`map[runID]*Run{cancel, doneCh, meta}`。`SendMessage` 用 `context.Background()` + trace/session 元数据派生 turn ctx，cancel 存入 registry；HTTP ctx 只影响 SSE 写出，不再传导进 agent loop。

run id 直接用发起请求的 trace_id（`TraceMiddleware` 已 mint、且 `client_msg_id = {trace_id}:{msg_index}` 体系已存在）——不新增 id 空间，取消端点收的就是 trace_id，观测链路天然对齐。下发渠道：`X-Run-Id` 响应头 + 首个 SSE 事件（`agent_start`）的 `meta.run_id`。

*备选*：新 mint UUID 作 run id。否决——与 trace_id 双轨增加关联成本，无收益。

### D2: Redis Stream 是事件日志的唯一权威，所有 SSE 连接都是它的订阅者

适配层的事件收集 goroutine（现 `TurnEventTap` 服务的那条）仍是 Hub 的唯一消费者，追加一个职责：每个事件 `XADD run:{rid}:events`（best-effort，失败仅 WARN，绝不阻塞 turn——沿用"compaction 永不阻塞"惯例）。`done` 事件也进日志（前端续传需要终局信号），但 messages 持久化过滤逻辑不变。

发起方 POST 的 SSE 写出与恢复端点的 SSE 写出统一为**同一个订阅实现**：从 `run:{rid}:events` 按 cursor 读（`XRANGE` 回放 + `XREAD BLOCK` 追 live），逐事件写 SSE。不建进程内 fan-out 双通道——单一数据路径，天然支持多标签页多订阅者，顺序由 stream 保证。

SSE `id:` 行 = Redis stream entry id（`{ms}-{seq}`，Redis 侧单调），`Last-Event-ID` 续传精确到事件，无重放/追live边界缝隙（回放到 last id 后从该 id XREAD，不会丢也不会重）。

*备选*：进程内 fan-out channel + Redis 只做重放日志。否决——两条路径两套顺序语义，且跨副本 attach 仍要 Redis 兜底。

### D3: session 互斥 = Redis `SET NX` 认领，409 响应携带 run_id

`SET session:{sid}:run {rid} NX EX 1800` 作为"该 session 有运行中 turn"的判据与互斥锁：抢到才开 turn，turn 结束 `DEL`。未抢到 → 409 `SESSION_BUSY`，body 带 `run_id`，前端可直接 attach。该 key 同时是 `GET /sessions` 列表 `generating` 标记与 attach 端点发现 active run 的来源——一个 key 三用，跨副本一致。

认领放在 session 属主校验之后、历史恢复之前；开 turn 失败（orchestrator 构建/panic 前错误）必须释放认领，不能把 session 锁死。

### D4: 取消三态：本进程 / 他副本 / 死 run

- **本进程**：registry 命中 → 直接 `cancel()` → 走现有 partial persist 路径 → 正常收尾清理。
- **他副本**（registry 未命中但 `run:{rid}:alive` 心跳还在）：`SET run:{rid}:cancel 1`；运行侧 drainer 每个心跳 tick（5s）轮询该标志，见 1 即 cancel 本地 turn ctx。跨副本取消延迟 ≤ 心跳周期。
- **死 run**（心跳过期、status 仍 running）：属主校验通过后强制清理——meta 标 `interrupted`、`DEL session:{sid}:run` 与相关 key。这就是"释放 context 资源"对死 run 的语义。

属主校验统一为：`run:{rid}:meta` 的 `user_id`/`session_id` 必须匹配 JWT 用户与路径 session，不匹配 → 404。

心跳：drainer 每 5s `SETEX run:{rid}:alive 15 {rid}`（TTL 3×周期）。它同时是 attach 端点判死依据（见 D6）。

### D5: 生命周期与清理——终局写日志，短保留窗口后删 key

turn 终局（done/error/cancel）：drainer 把 `done`/`agent_error` 终结事件 XADD 完，meta 写终态（`done`/`error`/`cancelled`），`DEL session:{sid}:run`（解除 409），**保留** `run:{rid}:events`/`meta` 一个固定 60s 窗口供晚到 attach 回放终局，随后 DEL；30min TTL 兜底一切异常路径（进程崩溃、清理代码失败）。

持久化照旧：终局后 `SaveMessagesBatch` 异步批写（saveCtx 已是 detached context，天然不受影响）。

### D6: attach 端点与死 turn 的终局判定

`GET /api/v1/sessions/:sid/turns/:rid/events`（JWT + 属主校验）：

- meta 存在且 status running → 回放 `XRANGE`（或 `Last-Event-ID` 之后）→ `XREAD BLOCK` 追 live。
- status 已终态 → 全量回放后由终局事件（`done`/`agent_error`）自然关流（60s 窗口内）。
- 追 live 期间每读周期检查：status 已终态（读完尾部后关流）或 `run:{rid}:alive` 过期且 status 仍 running → 判死：本地合成一个 `done`（meta.error 标 `interrupted`，语义对齐现有 done.error）写给学生端后关流，并把 meta 标 `interrupted`、`DEL session:{sid}:run`（attach 侧也能解锁 session，与 D4 死 run 清理收敛）。
- key 都不存在（窗口已过/从未有）→ 410 GONE，前端回落到普通历史读取。

### D7: 优雅关闭 = 有界取消全部运行中 turn

`serve` 的 graceful shutdown 里遍历 registry 逐个 cancel（各 turn 走现有 partial persist），等待 bounded（复用现有 shutdown timeout）。*备选*：等待自然结束。否决——turn 时长上界不确定，关闭时间不可控。

### D8: Redis key 总览

| Key | 类型 | TTL | 写者 |
|---|---|---|---|
| `run:{rid}:events` | Stream（`MAXLEN ~ 100000` 防失控） | 30min | drainer |
| `run:{rid}:meta` | Hash：session_id/user_id/status/model/created_at | 30min | drainer（状态迁移） |
| `run:{rid}:alive` | String | 15s，每 5s 续 | drainer 心跳 |
| `run:{rid}:cancel` | String | 30min | cancel 端点；drainer 消费后 DEL |
| `session:{sid}:run` | String = rid | 30min | SendMessage 认领 / 终局 DEL |

## Risks / Trade-offs

- [关页面继续烧 token（无条件新行为，产品成本语义变化）] → 唯一缓解是取消端点；文档明确写出，前端在会话列表提供取消入口（`generating` 标记）。
- [逐 token XADD 的 Redis 压力与日志体积] → token 事件速率 ≪ Redis 写吞吐（对比 msgflush/llmraw 的既有写入量）；`MAXLEN 100000` + 30min TTL 兜底。若实测有压力，drainer 内微批（攒 N ms 一次 pipeline XADD）是纯实现层优化，不动本设计。
- [Redis 故障时 run 日志不可用] → XADD best-effort：turn 照跑、持久化照走（saveCtx 路径不依赖 run 日志），只是该 turn 不可恢复续传；`session:{sid}:run` 认领失败时降级为"无互斥直接执行"还是"503 拒绝"需在 tasks 里定——倾向 503（宁可拒绝也不并发写同一 session）。
- [XADD 与 messages 持久化是两份中间态] → 明确分工：run 日志服务续传（事件粒度、终局即删），`msgs:` 体系服务历史（合并行、长期）；互不替代。
- [60s 保留窗口后 attach 返回 410，前端需回落历史] → 410 语义写进 openapi；前端在收到 410 时重拉 `GET /messages`（此时 SaveMessagesBatch 已入队，读路径有同步 drain 兜底，读到的即完整）。
- [发布/回滚时的在跑 turn] → 滚动重启 = D7 取消 + partial 持久化，语义等同用户显式取消；回滚旧版本 = 恢复"断开即取消"，无数据不兼容（run key 无人再读，30min TTL 自然清场）。

## Migration Plan

无 schema 变更、无配置变更。部署即生效（新行为无条件）。回滚 = 回退二进制，Redis 遗留 key 由 TTL 自清。前端需要同步升级：处理 409 `SESSION_BUSY` + `X-Run-Id` + attach 端点（openapi.yaml 更新后在 blowball-frontend 重跑 `npm run generate-api`）。

## Open Questions

- ~~`session:{sid}:run` 认领在 Redis 不可用时的降级（503 vs 无锁执行）~~ —— 实现期决策：**降级放行**（WARN + 无锁执行）。503 会破坏存量 message-write-behind 契约（"Redis 故障 → 消息直写 MySQL、对话继续"），认领是尽力而为的互斥而非准入控制。
- 60s 保留窗口与 5s 心跳周期是否需要做成配置（当前倾向写死常量，出现实际诉求再提配置）。
