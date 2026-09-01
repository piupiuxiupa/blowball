# Delta: turn-run-lifecycle

## MODIFIED Requirements

### Requirement: Session 单活动 run 互斥

系统 SHALL 以 Redis 原子认领（`SET session:{sid}:run {rid} NX`，带 TTL）保证一个 session 同时至多一个运行中 turn。认领 TTL SHALL 固定为 30 分钟，由独立常量（`SessionClaimTTL`）定义，SHALL NOT 随 run-key 家族的兜底 TTL 调整而联动。运行期间系统 SHALL 随心跳周期性重臂认领 TTL（仅当认领仍指向本 run 时续期），使互斥存续跟随 turn 心跳：合法长 turn SHALL NOT 因 TTL 到期而提前失去互斥，认领 TTL 的兜底语义 SHALL 为「最后一次心跳后 30 分钟」。认领失败时 SHALL 返回 409 `SESSION_BUSY` 且 body 携带当前运行中 turn 的 `run_id`。

认领 SHALL 在 turn 终局的**终局消息批次持久化完成后**释放（而非终局即刻释放）：终局批次指该 turn 的用户消息行（当发送时批次失败时）与全部 assistant 事件消息批。终局持久化 SHALL 在 turn goroutine 内同步执行，带有界超时与进程内退避重试；重试期间心跳 SHALL 持续维持（run 不得因持久化耗时被存活检测误判为死 run）。持久化预算耗尽后系统 SHALL 记录 ERROR 并继续终局释放（不得无限锁死 session）。认领 SHALL 在 turn 启动失败路径（如 orchestrator 构建失败）同样释放（该路径无消息批次，立即释放语义不变）。

#### Scenario: 运行中 session 再发消息被拒

- **WHEN** session 已有运行中 turn，用户再向该 session POST 消息
- **THEN** 系统返回 HTTP 409，body 为 `{"error": {"code": "SESSION_BUSY", "message": ..., "run_id": "<当前 run id>"}}`
- **AND** 运行中 turn 不受影响

#### Scenario: turn 终局后 session 解锁

- **WHEN** 运行中 turn 到达终局（done/error/cancel）且终局消息批次持久化完成
- **THEN** `session:{sid}:run` 认领被释放，后续向该 session 的消息请求被正常接受
- **AND** 客户端观察到 `generating` 翻为 false 时，该 turn 的消息批次已 durable

#### Scenario: 终局持久化完成前认领不释放

- **WHEN** turn 已到达终局但终局消息批次尚未持久化（如存储退避重试中）
- **THEN** `session:{sid}:run` 认领仍被持有，新消息请求返回 409 `SESSION_BUSY`
- **AND** persist 期间 run 心跳持续，attach 端不将该 run 判定为死 run

#### Scenario: 持久化预算耗尽后有界释放

- **WHEN** 终局持久化在有界预算（`turnPersistBudget` = 30s，至多 `turnPersistAttempts` = 3 次，退避 `turnPersistRetryBackoff` = 1s）内始终失败
- **THEN** 系统记录含批次规模的 ERROR 日志，随后执行终局释放（终态写入、claim 释放、retire）
- **AND** session 不会因存储故障被无限锁死

#### Scenario: turn 启动失败释放认领

- **WHEN** 消息请求已认领 session 但 turn 未成功启动（orchestrator 构建失败等）
- **THEN** 认领被释放，session 不会因失败请求被锁死

#### Scenario: Redis 认领不可达时降级放行

- **WHEN** 认领操作因 Redis 故障无法执行
- **THEN** 系统 WARN 后无锁执行该 turn（不拒绝请求），与消息持久化的直写降级一致；该 turn 的取消/恢复能力降级但生成不受影响

#### Scenario: 认领 TTL 独立于 run-key 兜底 TTL

- **WHEN** run-key 家族的 TTL 兜底基准值被调整（如本次提升至 1 小时）
- **THEN** 认领 TTL 保持 30 分钟不变，崩溃后的 session 兜底解锁时长不随之变化

#### Scenario: 长 turn 不因 TTL 到期提前失去互斥

- **WHEN** turn 运行超过 30 分钟且心跳持续（每 5 秒一次）
- **THEN** 认领 TTL 随心跳持续重臂，期间向该 session 的新消息仍返回 409 `SESSION_BUSY`；turn 到达终局且终局批次持久化完成后认领被主动释放（不等待 TTL）

#### Scenario: 心跳重臂不延长他人认领

- **WHEN**某 run 的心跳重臂执行时，认领 key 已不指向该 run（已被释放、强制清除或被新 run 认领）
- **THEN** 重臂为 no-op，不延长其他 run 的认领

### Requirement: Run 资源清理与优雅关闭

turn 到达终局时系统 SHALL：先在 turn goroutine 内完成终局消息批次持久化（见 Session 单活动 run 互斥的时序要求，可含有界同步 drain 使 MySQL 在 claim 释放前持有该批），再将终态写入 meta、释放 session 认领、在固定保留窗口（60 秒）后清理 run 的事件日志与 meta（崩溃残留时由 1 小时基准 ±10% 抖动的 TTL 兜底）。进程优雅关闭时 SHALL 在有界时间内取消全部运行中 turn（各 turn 走 interrupted-turn 持久化路径），且停机等待 SHALL 覆盖各 turn 的终局持久化完成——不得依赖固定 sleep 启发式估测持久化 goroutine 的完成。

#### Scenario: 终局后的保留窗口与清理

- **WHEN** turn 到达终局且终局消息批次持久化完成
- **THEN** session 认领释放；run 事件日志保留 60 秒供晚到 attach，随后被清理

#### Scenario: 终局 drain 保证 MySQL 可见性

- **WHEN** 终局消息批次双写 Redis 成功后、claim 释放前
- **THEN** 系统执行一次有界（`turnEndDrainTimeout` = 5s）同步 drain，使该批在 MySQL 中落地
- **AND** drain 失败仅 WARN 并继续终局释放，由 flusher 周期兜底，不无限持锁

#### Scenario: 优雅关闭取消运行中 turn

- **WHEN** 进程收到关闭信号且存在运行中 turn
- **THEN** 系统在有界时间内逐个取消这些 turn，部分输出被持久化
- **AND** 停机等待持续到各 turn 的终局持久化完成（或停机上限），不使用固定 sleep 估测

#### Scenario: 崩溃残留 run 的事件可回放窗口不少于一小时

- **WHEN** 进程崩溃导致终局清理（Retire）未执行，`run:{rid}:events`/`run:{rid}:meta` 仅靠 TTL 兜底存续
- **THEN** 事件日志与 meta 至少存活约 54 分钟（抖动下限），期间 `GET turns/:run_id/events` 仍可回放、attach 仍可合成 interrupted 终局
