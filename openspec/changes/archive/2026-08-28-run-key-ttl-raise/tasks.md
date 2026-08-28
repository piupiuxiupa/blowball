# Tasks: run-key-ttl-raise

## 1. 常量拆分与抖动函数（internal/run）

- [x] 1.1 `internal/run/run.go`：新增 `SessionClaimTTL = 30 * time.Minute` 常量（注释说明「认领锁崩溃兜底，独立于 RunKeyTTL，勿联动调整」）；`RunKeyTTL` 提升为 `1 * time.Hour` 并收窄注释为「run-key 家族崩溃兜底基准值（写入时叠加抖动）」；更新包顶 key-family 文档注释中的 TTL 描述
- [x] 1.2 `internal/run/run.go`：新增 `RunKeyTTLWithJitter() time.Duration`（`math/rand/v2`，基准 ±10% 均匀分布，54–66min），附使用约束注释（仅 Redis 写入点调用；memstore 用基准值）
- [x] 1.3 `internal/run/memstore.go`：claim 过期（line 170）改用 `SessionClaimTTL`；events/meta 过期（lines 86/88/131/204）语义不变（跟随 `RunKeyTTL` 基准值，不抖动）

## 2. claim 心跳重臂（长 turn 提前解锁缺口修复）

- [x] 2.1 `internal/run/run.go`：`Store.Heartbeat` 接口签名增加 `sessionID string` 形参，契约注释更新（重臂 alive/events/meta + compare-and-rearm 认领 TTL 为 `SessionClaimTTL`）
- [x] 2.2 `internal/store/redis/runstore.go`：新增 compare-and-rearm Lua 脚本（`GET key == runID` 时才 `EXPIRE key SessionClaimTTL`，与 `releaseClaimScript` 守卫惯例同构）；`Heartbeat` pipeline 追加该 Eval
- [x] 2.3 `internal/run/memstore.go`：`Heartbeat` 在认领 holder 仍为本 run 时刷新 `claimExp`（否则 no-op）
- [x] 2.4 `internal/run/drain.go`：`StartDrainer` 签名增加 `sessionID string` 形参，beat 闭包传入 `Heartbeat`
- [x] 2.5 `internal/handler/message_stream.go:539`：`StartDrainer` 调用点补传 `sessionID`

## 3. Redis 写入点（internal/store/redis/runstore.go）

- [x] 3.1 `ClaimSession`（line 183）`SetNX` TTL 改用 `run.SessionClaimTTL`
- [x] 3.2 五处 events/meta/cancel TTL 写入点改用 `run.RunKeyTTLWithJitter()`：`AppendEvent` 重臂（line 63）、`InitMeta`（line 139）、`SetStatus`（line 153）、`Heartbeat` 重臂（lines 234-235）、cancel `SET`（line 253）
- [x] 3.3 更新 runstore.go 顶部 key-family 注释中的 TTL 表述（30 分钟 → 1h ±10% 抖动、claim 30min 独立且随心跳重臂）

## 4. 测试

- [x] 4.1 `internal/run/run_test.go`：拆分/新增断言 —— `SessionClaimTTL == 30min`、`RunKeyTTL == 1h`；`RunKeyTTLWithJitter()` 多次采样落在 `[54min, 66min]` 区间且样本存在离散（非恒等基准值）
- [x] 4.2 `internal/store/redis/runstore_test.go`：钉死 30min 的 TTL 断言改为引用 `run.SessionClaimTTL`（claim）或 `[54min, 66min]` 区间断言（events/meta/cancel，miniredis TTL 读取）；新增同批创建多个 run key 的 TTL 互异断言（抖动生效）
- [x] 4.3 `internal/store/redis/runstore_test.go`：心跳重臂断言 —— claim 后 TTL 随 `Heartbeat` 重置为 `SessionClaimTTL`（holder 匹配时）；认领被释放/指向他人时重臂 no-op（他人认领 TTL 不变）
- [x] 4.4 `internal/run/memstore_test.go`（或既有 memstore 测试）：`Heartbeat` 刷新 `claimExp`、holder 不匹配时不刷新
- [x] 4.5 全量回归：`make test`（含 `test/integration/`）通过

## 5. 文档

- [x] 5.1 CLAUDE.md：更新「30min TTL backstops crashes」相关表述（agent orchestration 流程段 + Persistence 段），写明 claim 30min 独立常量、随心跳 compare-and-rearm（长 turn 不提前解锁）、events/meta/cancel 1h ±10% 抖动
