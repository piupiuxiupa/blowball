# turn-run-lifecycle Delta

## MODIFIED Requirements

### Requirement: Session 单活动 run 互斥

系统 SHALL 以 Redis 原子认领（`SET session:{sid}:run {rid} NX`，带 TTL）保证一个 session 同时至多一个运行中 turn——该单槽不变量 SHALL NOT 因消息排队能力而改变（排队只改变忙时请求的去向，不改变互斥本身）。认领 TTL SHALL 固定为 30 分钟，由独立常量（`SessionClaimTTL`）定义，SHALL NOT 随 run-key 家族的兜底 TTL 调整而联动。运行期间系统 SHALL 随心跳周期性重臂认领 TTL（仅当认领仍指向本 run 时续期），使互斥存续跟随 turn 心跳：合法长 turn SHALL NOT 因 TTL 到期而提前失去互斥，认领 TTL 的兜底语义 SHALL 为「最后一次心跳后 30 分钟」。

认领失败（已有 holder）时：`messages.queue.enabled` 为 false（默认）SHALL 返回 409 `SESSION_BUSY` 且 body 携带当前运行中 turn 的 `run_id`；为 true SHALL 按 session-message-queue 能力将消息入队（无队列可用时不产生 409）。

认领 SHALL 在 turn 终局的**终局消息批次持久化完成后**释放（而非终局即刻释放）：终局批次指该 turn 的用户消息行（当发送时批次失败时）与全部 assistant 事件消息批。终局持久化 SHALL 在 turn goroutine 内同步执行，带有界超时与进程内退避重试；重试期间心跳 SHALL 持续维持（run 不得因持久化耗时被存活检测误判为死 run）。持久化预算耗尽后系统 SHALL 记录 ERROR 并继续终局释放（不得无限锁死 session）。认领 SHALL 在 turn 启动失败路径（如 orchestrator 构建失败）同样释放（该路径无消息批次，立即释放语义不变）。终局释放之后、turn goroutine 结束之前，系统 SHALL 按 session-message-queue 能力的接续驱动要求尝试接续队列队头（优雅停机时跳过，认领被占则推回队首）。

#### Scenario: 运行中 session 再发消息被拒（队列禁用）

- **WHEN** session 已有运行中 turn，`messages.queue.enabled` 为 false（默认），用户再向该 session POST 消息
- **THEN** 系统返回 HTTP 409，body 为 `{"error": {"code": "SESSION_BUSY", "message": ..., "run_id": "<当前 run id>"}}`
- **AND** 运行中 turn 不受影响

#### Scenario: 运行中 session 再发消息入队（队列启用）

- **WHEN** session 已有运行中 turn，`messages.queue.enabled` 为 true，用户再向该 session POST 消息
- **THEN** 消息按 session-message-queue 能力入队（认领单槽仍由运行中 turn 持有，请求不被 409 拒绝）

#### Scenario: turn 终局后 session 解锁

- **WHEN** 运行中 turn 到达终局（done/error/cancel）且终局消息批次持久化完成
- **THEN** `session:{sid}:run` 认领被释放；队列禁用时后续向该 session 的消息请求被正常接受，队列启用时队头消息被接续为下一轮
- **AND** 客户端观察到 `generating` 翻为 false 时（无接续或接续尚未认领的间隙），该 turn 的消息批次已 durable

#### Scenario: 终局持久化完成前认领不释放

- **WHEN** turn 已到达终局但终局消息批次尚未持久化（如存储退避重试中）
- **THEN** `session:{sid}:run` 认领仍被持有，新消息请求不得认领该会话的 run 槽（队列禁用时返回 409 `SESSION_BUSY`，启用时入队）
- **AND** persist 期间 run 心跳持续，attach 端不将该 run 判定为死 run

#### Scenario: 持久化预算耗尽后有界释放

- **WHEN** 终局持久化在有界预算（`turnPersistBudget` = 30s，至多 `turnPersistAttempts` = 3 次，退避 `turnPersistRetryBackoff` = 1s）内始终失败
- **THEN** 系统记录含批次规模的 ERROR 日志，随后执行终局释放（终态写入、claim 释放、retire）
- **AND** session 不会因存储故障被无限锁死

#### Scenario: turn 启动失败释放认领

- **WHEN** 消息请求已认领 session 但 turn 未成功启动（orchestrator 构建失败等）
- **THEN** 认领被释放，session 不会因失败请求被锁死
- **AND** 队列启用时释放后照常尝试接续队头

#### Scenario: Redis 认领不可达时降级放行

- **WHEN** 认领操作因 Redis 故障无法执行
- **THEN** 系统 WARN 后无锁执行该 turn（不拒绝请求），与消息持久化的直写降级一致；该 turn 的取消/恢复能力降级但生成不受影响
- **AND** 队列启用时入队原子操作同样不可用，按同一降级语义直接起轮

#### Scenario: 认领 TTL 独立于 run-key 兜底 TTL

- **WHEN** run-key 家族的 TTL 兜底基准值被调整（如本次提升至 1 小时）
- **THEN** 认领 TTL 保持 30 分钟不变，崩溃后的 session 兜底解锁时长不随之变化

#### Scenario: 长 turn 不因 TTL 到期提前失去互斥

- **WHEN** turn 运行超过 30 分钟且心跳持续（每 5 秒一次）
- **THEN** 认领 TTL 随心跳持续重臂，期间向该 session 的新消息不得认领 run 槽（队列禁用时返回 409 `SESSION_BUSY`，启用时入队）；turn 到达终局且终局批次持久化完成后认领被主动释放（不等待 TTL）

#### Scenario: 心跳重臂不延长他人认领

- **WHEN**某 run 的心跳重臂执行时，认领 key 已不指向该 run（已被释放、强制清除或被新 run 认领）
- **THEN** 重臂为 no-op，不延长其他 run 的认领

### Requirement: 按 run id 取消运行中 turn

系统 SHALL 提供取消端点（JWT 鉴权），接收 run id 并取消对应运行中请求、释放其资源。取消 SHALL 复用现有 interrupted-turn 持久化路径持久化部分输出。取消 SHALL 先校验 run 归属（meta 中的 user_id/session_id 与调用者匹配），不匹配返回 404。四种取消情形 SHALL 分别成立：本进程运行中（registry 直接 cancel）、其他副本运行中（写取消标志，运行侧在心跳周期内观察到并取消）、进程已死（心跳过期且状态仍为 running → 标记 interrupted 并强制清理、解锁 session）、排队中（状态为 queued → 按 session-message-queue 能力出队取消，终态 cancelled）。排队中的 run SHALL NOT 落入死 run 强清路径（其无心跳属正常状态）。取消运行中 turn SHALL NOT 终止该会话队列中的其余排队条目。

#### Scenario: 取消本进程运行中 turn

- **WHEN** 用户对运行中 turn 调用取消端点，且该 run 在当前进程 registry 中
- **THEN** turn 被取消，部分输出按 interrupted-turn 路径持久化
- **AND** run 状态置为 cancelled，session 认领释放

#### Scenario: 取消其他副本上的 turn

- **WHEN** 取消端点所在进程 registry 未命中该 run，但 run 心跳仍存活
- **THEN** 系统写入取消标志，运行该 turn 的进程在心跳周期内取消 turn，终态为 cancelled

#### Scenario: 清理死 run

- **WHEN** 取消端点发现 run 心跳已过期且状态仍为 running
- **THEN** run 状态标记为 interrupted，session 认领释放，相关 Redis key 清理

#### Scenario: 取消排队中的消息

- **WHEN** 用户对状态为 queued 的 run 调用取消端点
- **THEN** 队列条目被移除，run 状态置为 cancelled（不标记 interrupted）
- **AND** 该会话队列中其余条目不受影响

#### Scenario: 取消他人 run

- **WHEN** 用户调用的 run id 其 meta 归属（user_id/session_id）与调用者不匹配
- **THEN** 系统返回 HTTP 404，且 run 状态不变
