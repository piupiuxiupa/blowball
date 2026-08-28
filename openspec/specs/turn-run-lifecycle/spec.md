# turn-run-lifecycle Specification

## Purpose

定义与发起 SSE 连接解耦的服务端 turn 生命周期能力：run 身份签发与下发（trace_id 即 run id，经 `X-Run-Id` 响应头与 `meta.run_id` 下发）、session 单活动 run 互斥（Redis 原子认领，忙时 409 SESSION_BUSY）、按 run id 的取消端点与事件流恢复端点（重放 + live 续传）、Redis Stream 事件日志 + run 元数据 + 心跳的运行状态保存、进程崩溃的中断语义（心跳过期 → interrupted），以及终局后的保留窗口资源清理与优雅关闭取消。
## Requirements
### Requirement: Turn 执行独立于发起连接的生命周期

系统 SHALL 在与发起 HTTP 请求解耦的服务端上下文中执行 turn：SSE 连接断开（页面关闭、网络中断、客户端主动断流）SHALL NOT 取消或暂停 turn。turn SHALL 仅因以下原因终止：正常完成（done）、orchestrator 错误（error）、显式取消（cancel）或进程终止。

#### Scenario: 客户端断开后生成继续

- **WHEN** turn 流式输出进行中，发起 SSE 连接断开
- **THEN** turn 继续执行直到终局（done/error），断开后产生的事件继续写入该 run 的事件日志
- **AND** 该 turn 的消息持久化（`SaveMessagesBatch`）照常发生

#### Scenario: 断开后重连可看到全部输出

- **WHEN** 客户端断开后通过恢复端点重新 attach 同一 run
- **THEN** 客户端可回放断开期间错过的事件并继续接收 live 输出

### Requirement: Run 身份签发与下发

系统 SHALL 为每个被接受的消息请求签发 run id，其值 SHALL 等于该请求的 trace_id。run id SHALL 通过 `X-Run-Id` 响应头与首个 `agent_start` 事件的 `meta.run_id` 下发。

#### Scenario: 发起请求获得 run id

- **WHEN** 用户发送 POST /api/v1/sessions/:session_id/messages 且该 session 无运行中 turn
- **THEN** SSE 响应携带 `X-Run-Id` 头，值为该请求的 trace_id
- **AND** 首个 `agent_start` 事件的 `meta.run_id` 携带相同值

### Requirement: Session 单活动 run 互斥

系统 SHALL 以 Redis 原子认领（`SET session:{sid}:run {rid} NX`，带 TTL）保证一个 session 同时至多一个运行中 turn。认领 TTL SHALL 固定为 30 分钟，由独立常量（`SessionClaimTTL`）定义，SHALL NOT 随 run-key 家族的兜底 TTL 调整而联动。运行期间系统 SHALL 随心跳周期性重臂认领 TTL（仅当认领仍指向本 run 时续期），使互斥存续跟随 turn 心跳：合法长 turn SHALL NOT 因 TTL 到期而提前失去互斥，认领 TTL 的兜底语义 SHALL 为「最后一次心跳后 30 分钟」。认领失败时 SHALL 返回 409 `SESSION_BUSY` 且 body 携带当前运行中 turn 的 `run_id`。认领 SHALL 在 turn 终局释放， SHALL 在 turn 启动失败路径（如 orchestrator 构建失败）同样释放。

#### Scenario: 运行中 session 再发消息被拒

- **WHEN** session 已有运行中 turn，用户再向该 session POST 消息
- **THEN** 系统返回 HTTP 409，body 为 `{"error": {"code": "SESSION_BUSY", "message": ..., "run_id": "<当前 run id>"}}`
- **AND** 运行中 turn 不受影响

#### Scenario: turn 终局后 session 解锁

- **WHEN** 运行中 turn 到达终局（done/error/cancel）
- **THEN** `session:{sid}:run` 认领被释放，后续向该 session 的消息请求被正常接受

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
- **THEN** 认领 TTL 随心跳持续重臂，期间向该 session 的新消息仍返回 409 `SESSION_BUSY`；turn 到达终局时认领被立即主动释放（不等待 TTL）

#### Scenario: 心跳重臂不延长他人认领

- **WHEN**某 run 的心跳重臂执行时，认领 key 已不指向该 run（已被释放、强制清除或被新 run 认领）
- **THEN** 重臂为 no-op，不延长其他 run 的认领

### Requirement: 按 run id 取消运行中 turn

系统 SHALL 提供取消端点（JWT 鉴权），接收 run id 并取消对应运行中请求、释放其资源。取消 SHALL 复用现有 interrupted-turn 持久化路径持久化部分输出。取消 SHALL 先校验 run 归属（meta 中的 user_id/session_id 与调用者匹配），不匹配返回 404。三种取 SHALL 分别成立：本进程运行中（registry 直接 cancel）、其他副本运行中（写取消标志，运行侧在心跳周期内观察到并取消）、进程已死（心跳过期且状态仍为 running → 标记 interrupted 并强制清理、解锁 session）。

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

#### Scenario: 取消他人 run

- **WHEN** 用户调用的 run id 其 meta 归属（user_id/session_id）与调用者不匹配
- **THEN** 系统返回 HTTP 404，且 run 状态不变

### Requirement: 恢复端点重放并续传运行中 turn 的事件流

系统 SHALL 提供恢复端点 `GET /api/v1/sessions/:session_id/turns/:run_id/events`（JWT + 归属校验），以 SSE 返回该 run 的事件：先重放已有事件日志（从头，或从 `Last-Event-ID` 指定的事件之后），再追 live 输出直至终局。SSE 的 `id:` 行 SHALL 等于事件在 Redis Stream 中的 entry id。多个并发订阅（多标签页）SHALL 互不干扰。事件日志 SHALL 只含 agent 事件——用户消息行不进事件日志（`message` 为持久化 sentinel，不上 SSE）；重连侧的完整视图 SHALL 由历史读取（含该 turn 发送时已持久化的用户消息行，见 session-management / message-write-behind 能力）与事件重放共同构成。

#### Scenario: 重开会话自动恢复

- **WHEN** 用户重开一个存在运行中 turn 的 session 并 attach 其 run
- **THEN** 端点回放自开始以来的全部事件，随后继续推送 live 事件直到终局
- **AND** 客户端经历史读取可见该 turn 的用户消息行（发送时已持久化），与事件重放共同构成含用户提问与 agent 输出的完整视图

#### Scenario: Last-Event-ID 断点续传

- **WHEN** 客户端携带 `Last-Event-ID` attach
- **THEN** 端点从该事件之后继续推送，无事件丢失且无重复

#### Scenario: attach 已终局的 run

- **WHEN** run 已终局但仍处保留窗口内
- **THEN** 端点回放全部事件（含终局事件）后关闭流

#### Scenario: attach 超出保留窗口的 run

- **WHEN** run 的 Redis key 已被清理（保留窗口已过或从未存在）
- **THEN** 端点返回 HTTP 410，客户端回落普通历史读取

### Requirement: Run 事件日志与状态保存于 Redis

系统 SHALL 将 turn 的全部事件（含 `done` 终局事件）逐事件追加到 Redis Stream `run:{rid}:events`，并将 run 元数据（session_id、user_id、status、model、created_at）保存于 `run:{rid}:meta`。运行期间系统 SHALL 周期性刷新 run 心跳（`run:{rid}:alive`，短 TTL）。`run:{rid}:events` / `run:{rid}:meta` / `run:{rid}:cancel` SHALL 有 TTL 兜底，基准值为 1 小时，每次写入 SHALL 施加 ±10% 的随机抖动（实际 TTL 落在 54–66 分钟），以避免 key 齐步失效；活跃 run 的 TTL SHALL 由心跳周期性重臂（同样带抖动）使长 turn 不中途过期。Stream SHALL 有长度上限，事件追加失败 SHALL 仅告警、SHALL NOT 阻塞或中断 turn。心跳 key（`run:{rid}:alive`）的短 TTL SHALL 保持不变（15 秒），SHALL NOT 参与抖动。

#### Scenario: 事件逐条入日志且含终局

- **WHEN** turn 产生事件（token/tool_call/tool_result/agent_*/done）
- **THEN** 每个事件作为一条 Stream entry 追加到 `run:{rid}:events`，`done` 事件同样入日志

#### Scenario: Redis 追加失败不阻断 turn

- **WHEN** XADD 失败（Redis 暂不可用）
- **THEN** 系统记录 WARN 并继续 turn；turn 的消息持久化不受影响（该 turn 不可恢复续传）

#### Scenario: run key TTL 带抖动且不早于一小时基准的下限

- **WHEN** 任一写入点设置 `run:{rid}:events` / `run:{rid}:meta` / `run:{rid}:cancel` 的 TTL
- **THEN** 实际 TTL 为 1 小时基准叠加 ±10% 均匀抖动（54–66 分钟），同批创建的 key 过期时间相互错开

#### Scenario: 心跳重臂维持长 turn 存活

- **WHEN** turn 运行超过 TTL 基准值（>1 小时）
- **THEN** 心跳周期性重臂 events/meta 的 TTL，run 中途不过期；心跳 key 自身 TTL 保持 15 秒

### Requirement: 进程崩溃的中断语义

进程死亡时该进程上的运行中 turn SHALL 视为中断：事件日志保留可回放，run 状态 SHALL 由存活检测方（attach 端点或取消端点）在心跳过期后标记为 interrupted 并释放 session 认领；TTL 到期 SHALL 作为最终兜底解锁。

#### Scenario: attach 检测到死 run

- **WHEN** 客户端 attach 一个状态仍为 running 但心跳已过期的 run
- **THEN** 端点回放已入日志的事件后，合成携带 interrupted 错误信息的终局 `done` 事件并关流
- **AND** run 状态标记为 interrupted，session 认领释放

### Requirement: Run 资源清理与优雅关闭

turn 到达终局时系统 SHALL：将终态写入 meta、释放 session 认领、在固定保留窗口（60 秒）后清理 run 的事件日志与 meta（崩溃残留时由 1 小时基准 ±10% 抖动的 TTL 兜底）。进程优雅关闭时 SHALL 在有界时间内取消全部运行中 turn（各 turn 走 interrupted-turn 持久化路径）。

#### Scenario: 终局后的保留窗口与清理

- **WHEN** turn 到达终局
- **THEN** session 认领立即释放；run 事件日志保留 60 秒供晚到 attach，随后被清理

#### Scenario: 优雅关闭取消运行中 turn

- **WHEN** 进程收到关闭信号且存在运行中 turn
- **THEN** 系统在有界时间内逐个取消这些 turn，部分输出被持久化

#### Scenario: 崩溃残留 run 的事件可回放窗口不少于一小时

- **WHEN** 进程崩溃导致终局清理（Retire）未执行，`run:{rid}:events`/`run:{rid}:meta` 仅靠 TTL 兜底存续
- **THEN** 事件日志与 meta 至少存活约 54 分钟（抖动下限），期间 `GET turns/:run_id/events` 仍可回放、attach 仍可合成 interrupted 终局
