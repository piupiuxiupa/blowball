# Design: run-key-ttl-raise

## Context

`internal/run/run.go` 定义 `RunKeyTTL = 30 * time.Minute`，被两类语义不同的机制共用：

| 消费方 | 语义 | 站点 |
|---|---|---|
| session-run 认领锁（`SET session:{sid}:run {rid} NX`） | 崩溃后 session 最多被锁多久（`SESSION_BUSY` 兜底解锁） | `runstore.go:183`、`memstore.go:170` |
| `run:{rid}:events` / `run:{rid}:meta` / `run:{rid}:cancel` | 崩溃残留 run 资源的 TTL 兜底（正常路径由 `Retire` 显式清理） | `runstore.go:63,139,153,234,235,253`、`memstore.go:86,88,131,204` |

活跃 run 的 TTL 由心跳每 5s 重臂（`Heartbeat`），长 turn 不会中途过期；`Retire` 在终局后将 events/meta 显式降到 60s 保留窗并删除 alive/cancel。`RunKeyTTL` 只在「没有终局清理」（进程崩溃）时生效。

运营要求：crash 后可回放窗口 ≥1h，且 TTL 带偏移避免齐步失效。认领锁必须维持 30min（放宽 = 崩溃后 session 被锁更久）。

## Goals / Non-Goals

**Goals:**

- 崩溃残留的 events/meta/cancel 兜底 TTL：30min → **1h 基准 ±10% 抖动**（均匀 54–66min）。
- 认领锁 TTL 与 run-key 兜底 TTL 解耦为两个常量，认领 TTL 数值不变（30min）。
- 每个写入点独立重掷抖动，key 过期时间自然错开。
- **修复长 turn 提前解锁缺口**：认领 TTL 纳入心跳重臂，互斥存续跟随 turn 心跳（最后一次心跳后 30 分钟），合法长 turn 不再于 30 分钟处静默失去互斥。

**Non-Goals:**

- 不动心跳 TTL（15s）、终局保留窗（60s）、Stream MAXLEN（100k）。
- 不动缓存族（`msgs:`/`session:`/`compaction:`，24h 平 TTL）与无 TTL 队列。
- 不引入任何配置项（常量级调整，与 HeartbeatTTL 等既有生命周期常量同为硬编码约定）。
- 不改变 `Retire`/attach/取消的任何逻辑分支。

## Decisions

### D1：拆常量而非参数化

新增 `SessionClaimTTL = 30 * time.Minute`；`RunKeyTTL` 提升为 `1 * time.Hour` 并将注释收窄为「run-key 家族的崩溃兜底基准值」。`ClaimSession`（redis + memstore 两处）改用前者。

*备选*：把 TTL 做成 `Store` 接口参数或配置项 —— 拒绝。两个数值都是生命周期语义常量（同 `HeartbeatEvery`/`HeartbeatTTL`/`RetainAfterTerminal` 一族），无运营调参需求；引入配置面反而诱惑误调认领锁。

### D2：抖动实现为 run 包的辅助函数，仅 Redis 实现注入

```go
// run 包
func RunKeyTTLWithJitter() time.Duration {
    // base ±10%：54min–66min 均匀分布
    return RunKeyTTL + time.Duration(rand.Int64N(int64(RunKeyTTL)/5)-int64(RunKeyTTL)/10)
}
```

Redis 侧 5 个写入点（`InitMeta`、`AppendEvent` 重臂、`SetStatus`、`Heartbeat` 重臂、cancel `SET`）每次调用取新值——同一 key 的 TTL 随每次心跳重掷，过期时间持续漂移。memstore（测试假实现）直接用 `RunKeyTTL` 基准值，保持测试确定性；抖动行为由 runstore 测试以范围断言覆盖。

*备选*：抖动放 `store/redis` 层 —— 拒绝。数值语义属于 run 包契约（接口文档全在 `run.go` 注释里），store 层只做机械映射。

*备选*：抖动改在 key 粒度缓存（同一 key 存续期固定偏移）—— 拒绝。TTL 每次写入本就刷新，随写重掷更简单且效果等价（齐步创建的 key 在首写即错开）。

### D3：随机源用 `math/rand/v2` 全局函数

`rand.Int64N`（v2）并发安全、无需播种、不产生 `math/rand` 的全局锁争议。抖动是尽力而为的偏移，无需可测试的注入点（测试断言范围即可）。

### D4：cancel key 跟随提升（含在范围内）

`run:{rid}:cancel` 由取消端点以 `RunKeyTTL` 设置，`Retire` 终局时会 `Del`。崩溃残留的 cancel key 多留 ≤1h 无害——唯一读者是已死 run 的 drainer 轮询。统一跟随基准值，避免第三个常量。

### D5：claim 纳入心跳重臂（compare-and-rearm），修复长 turn 提前解锁

现状缺口：`ClaimSession` 的 30min TTL 在认领时一次性定死，`Heartbeat` 重臂 alive/events/meta 却唯独不续 claim —— 合法运行超过 30 分钟的 turn 会在 30 分钟处静默失去互斥（新消息可认领并并发，消息顺序错乱、session 列表 `generating` 失真）。这不是有意取舍：`Heartbeat(ctx, runID)` 只拿 runID、拿不到 sessionID，重臂列表里自然放不进 claim；而 `ReleaseSession` 的 compare-and-delete 守卫说明「认领与 turn 生命期错位」早被意识到，只是防了误删、没防提前过期。

修复形态：

- **接口**：`Store.Heartbeat(ctx, runID, sessionID)`（签名加 sessionID）；`StartDrainer` 增加同名形参，调用点 `message_stream.go:539` 手里即有 `sessionID`，贯通无阻力。
- **Redis 实现**：心跳 pipeline 追加一条 Lua compare-and-rearm —— `GET key == runID` 时才 `EXPIRE key SessionClaimTTL`，与 `releaseClaimScript` 的守卫惯例同构。key 缺失（已释放/已被 force-clear）时为 no-op。
- **语义**：认领 TTL 的兜底起算点从「认领时刻」变为「最后一次心跳」。长 turn：心跳每 5s 续 → 锁跟随 turn 存活。崩溃：心跳停止 → 最后一次心跳 +30min 过期 —— 与现状的「认领 +30min」相差 ≤5s（一个心跳周期），崩溃兜底时长实质不变。正常终局：`ReleaseSession` 主动删除，时机与路径完全不变，TTL 不参与。

*备选*：认领 TTL 直接调大到覆盖最长 turn —— 拒绝。既堵死崩溃后恢复窗口，又没有上限保证（100 轮上限 × 每轮重试 × 71s MCP 调用可以远超任何预选值）。*备选*：发送路径 409 时顺带做死活检测 —— 拒绝。改的是错误路径的语义面，且心跳重臂是更小、更局部的修复。

## Risks / Trade-offs

- [崩溃残留 key 驻留时间 ×2（30min → ≤66min），Redis 内存微增] → Stream 有 MAXLEN 100k 上限、meta 是小 JSON；单个崩溃 run 的残留量本就有界，可接受。
- [`GET turns/:run_id/events` 对崩溃 run 的 410 时点推迟到 ~1h] → 与「至少保留一小时」的运营诉求同向，是特性不是回归。
- [抖动使 runstore 测试无法断言精确 TTL] → 测试改为断言 `[54min, 66min]` 区间；memstore 路径无抖动、断言基准值。
- [认领锁与 run-key 兜底解耦后，两常量可能被误改回同一个] → `run.go` 注释交叉引用两者语义（「认领锁兜底，勿随 RunKeyTTL 调整」），`run_test.go` 分别断言两个常量值。
- [漂移的重臂延长他人认领（陈旧 drainer 仍在跳、认领已被新 run 拿走）] → compare-and-rearm Lua 守卫：`GET != runID` 时不 EXPIRE；且重臂后本 change 的语义下该场景只在「旧 turn 心跳停止 → 认领过期 → 新认领」之后才可能发生，此时旧 drainer 已死，理论窗口为空，守卫是纯防御。
- [终局先释放、drainer 还有最后一次 beat → EXPIRE 落在已删 key 上] → Redis EXPIRE 对缺失 key 是 no-op，无副作用。

## Migration Plan

纯代码常量调整，无 schema/配置/API 变更，一次部署即生效。滚动部署期间新旧进程混跑无影响（TTL 是每次写入的值，不存跨进程一致性）。回滚 = 还原代码重新部署。

## Open Questions

（无 —— 决策点已在探索阶段与运营确认：心跳/认领保持原样、基准 1h、±10%。）
