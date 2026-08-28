# Proposal: run-key-ttl-raise

## Why

崩溃残留的 run key（`run:{rid}:events` / `run:{rid}:meta` / `run:{rid}:cancel`）目前统一以 30 分钟 TTL 兜底过期，运营侧要求 crash 后的可回放窗口**至少保留一个小时**，并希望 TTL 带随机偏移避免 key 齐步失效。同时现状是单一常量 `run.RunKeyTTL = 30min` 同时支撑两个语义不同的机制 —— 会话认领锁（崩溃后 session 最多被锁 30 分钟）与 run 资源保留兜底 —— 想只提高后者就必须先拆常量。

## What Changes

- **拆分常量**：新增 `run.SessionClaimTTL = 30min` 专供 session-run 认领锁（`runstore.ClaimSession` 与 `memstore` 对应实现各一处）；认领 TTL 数值不变（30 分钟），但同步纳入心跳重臂（见下条）。
- **提升 + 抖动**：`run:{rid}:events` / `run:{rid}:meta` / `run:{rid}:cancel` 的 TTL 兜底由 30 分钟提升为 **1 小时基准 ±10% 抖动**（均匀分布 54–66min），Redis 侧每个写入点独立重掷（`InitMeta`、`AppendEvent` 重臂、`SetStatus`、`Heartbeat` 重臂、cancel `SET`）。
- **claim 纳入心跳重臂（修复既有缺口）**：现状认领 TTL 在认领时一次性定死且无续期 —— 合法运行超过 30 分钟的 turn 会在 30 分钟处静默失去互斥（新消息可认领、与老 turn 并发，消息顺序错乱；session 列表的 `generating` 同时失真）。修复：`Heartbeat` 周期性以 compare-and-rearm（仅当认领仍指向本 run 时续期）将认领 TTL 续为 `SessionClaimTTL`。语义由「认领起 30 分钟定死」变为「**最后一次心跳后 30 分钟**」—— 长 turn 不再提前解锁；崩溃兜底时长不变（心跳停止后 ≤30min 解锁）；正常终局的主动释放（`ReleaseSession`，Lua compare-and-delete）路径与时机不变。
- **明确不动**：心跳 TTL 15s、终局保留窗 `RetainAfterTerminal` 60s、缓存族（`msgs:`/`session:`/`compaction:`）24h、无 TTL 队列（`msgs:buffer`/`msgs:processing`/`llm_raw:buffer`）。
- **行为后果**（设计上接受）：无 Retire 的崩溃 run，其事件回放窗口（`GET turns/:run_id/events`）与 attach 合成 `interrupted` 的判定窗口由 ≤30min 延长到 ≤~1h；cancel 孤儿 key 残留 ≤1h（无害——只有已死的 drainer 会读它）。正常终局路径的 60s 保留窗语义不变。
- **memstore**（测试用内存实现）使用基准值、不注入抖动，保证测试确定性。
- 无 API、schema、配置变更；纯内部常量与写入点调整。

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `turn-run-lifecycle`: 「全部 run key SHALL 有 TTL 兜底（30 分钟）」的要求改为 —— events/meta/cancel 的 TTL 兜底为 1 小时基准 ±10% 抖动；会话认领要求增加心跳重臂语义（TTL 固定 30 分钟、由独立常量定义、随心跳 compare-and-rearm 续期，长 turn 不提前失去互斥）；崩溃中断语义与终局清理要求中的「TTL 30 分钟兜底」表述同步更新。

## Impact

- `internal/run/run.go`：常量拆分（`SessionClaimTTL` 新增、`RunKeyTTL` 语义收窄为 run-key 家族基准值并提升到 1h）+ 抖动辅助函数 + `Store.Heartbeat` 接口签名增加 `sessionID`（契约注释同步）+ 包注释。
- `internal/run/drain.go`：`StartDrainer` 增加 `sessionID` 形参，beat 闭包传入 `Heartbeat`。
- `internal/handler/message_stream.go`：`StartDrainer` 调用点（:539）补传 `sessionID`。
- `internal/store/redis/runstore.go`：5 处 events/meta/cancel TTL 写入点改用抖动值；`ClaimSession` 改用 `SessionClaimTTL`；`Heartbeat` 增加 claim 的 compare-and-rearm（Lua：GET==runID 才 EXPIRE）。
- `internal/run/memstore.go`：claim 过期改用 `SessionClaimTTL`；events/meta 过期跟随基准值；`Heartbeat` 在 holder 匹配时刷新 `claimExp`。
- 测试：`internal/run/run_test.go`、`internal/store/redis/runstore_test.go` 中钉死 30min 的断言改为范围断言（54–66min）或引用新常量。
- 文档：CLAUDE.md 中「30min TTL backstops crashes」两处表述更新。
