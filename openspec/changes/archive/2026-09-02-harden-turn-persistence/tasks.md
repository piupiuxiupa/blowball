# Tasks: harden-turn-persistence

## 1. 错误语义与 send-time 兜底（D1）

- [x] 1.1 `internal/service/session.go`：`fallbackDirectWrite` 返回错误；`SaveMessagesBatch` 仅在「Redis 双写失败且 MySQL 直写失败」时返回该错误（Redis 单挂直写成功仍返回 nil），更新函数注释
- [x] 1.2 `internal/service/session_test.go`：新增双挂上抛、单挂降级返回 nil 两组单测（fake Redis/MySQL 注入失败）
- [x] 1.3 `internal/handler/message_stream.go`：确认 send-time 路径 `err != nil` 分支不执行 `markUserPersisted()`，终局批补写用户行路径随 1.1 自动复活；补 handler 层测试（send-time 双挂 → 终局批含用户行且 MySQL 无重复）

## 2. 终局持久化进入 turn 生命周期（D2 D3 D4）

- [x] 2.1 `internal/handler/message_stream.go`：将 `persistEvents` 的执行从 handler 侧异步 goroutine 挪入 turn goroutine（`<-drainDone` 之后、`Finalize` 之前），同步执行；memory capture 保持 fire-and-forget，turn_usage 随批写入且失败仅 WARN
- [x] 2.2 为终局持久化加有界重试（常量：总预算 30s、尝试 3 次、退避 1s），预算耗尽记录含 trace/session/批次规模的 ERROR 后继续 Finalize
- [x] 2.3 persist 阶段维持 run 心跳：turn goroutine 以 `HeartbeatEvery` 周期调用 `store.Heartbeat`（compare-and-rearm 语义），Finalize 前停止；防止 attach 端因 alive 过期误判死 run
- [x] 2.4 turn 生命周期测试：claim 在终局批 durable 前不释放、持久化预算耗尽仍有界释放、persist 期间 attach 不判死 run、done/取消/错误三条路径均先持久化后 Finalize

## 3. drain-before-release（D5，可裁剪）

- [x] 3.1 turn goroutine 在终局批 `SaveMessagesBatch` 成功后、Finalize 前执行一次有界（`turnEndDrainTimeout` 5s）同步 drain；失败仅 WARN 并继续释放
- [x] 3.2 测试：claim 释放时 MySQL 已含终局批（历史端点即时可见）；drain 失败不阻塞 Finalize 且 flusher 周期兜底

## 4. 优雅停机简化（D6）

- [x] 4.1 `cmd/blowball/serve.go`：移除 `time.Sleep(250ms)` 启发式及其注释，`WaitAll` 现在自然覆盖终局持久化；确认停机上限仍由 `ShutdownTimeout` 有界
- [x] 4.2 停机测试/集成验证：取消信号 → 终局持久化完成 → Finish 的顺序在 WaitAll 等待范围内

## 5. 回归与收尾

- [x] 5.1 `make test`（race）全绿；`make lint` 通过
- [x] 5.2 集成验证：正常 turn 终局后 `generating:false` 时历史端点完整；优雅停机后无终局批丢失；双挂注入下 ERROR 日志可见且用户行由终局批兜底
- [x] 5.3 对照 delta spec 逐场景核对（message-write-behind 双层失败上抛、turn-run-lifecycle 认领释放时序/心跳维持/有界释放/停机等待），更新 spec 中「默认量级」为实现的常量值
