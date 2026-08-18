## Context

当前消息持久化是三层同步串行（`SessionService.SaveMessagesBatch`）：Redis RPUSH（best-effort）→ FS 会话文件读改写（同步，失败即整体报错）→ MySQL 多值 INSERT（同步，失败仅记日志）+ `UpdateSessionTime`。每轮 turn 结束由 `message_stream.go` 的 detached-context goroutine 调用一次。读路径 `RecoverMessages` 按 Redis → FS → MySQL 降级并逐层回填；历史分页接口直接读 MySQL。

仓库内已有已验证的 write-behind 模板：`internal/llmraw`（`llm_raw:buffer` 无 TTL 队列 + 后台 Flusher，5s ticker / 队列阈值提前冲刷 / 优雅停机有界 drain / 多进程原子 `LPOP count` / 失败 at-most-once）。本设计将该模式移植到消息持久化，并针对"消息是业务数据"升级其投递语义。

关键既有事实（探索阶段确认）：

- 读序为 `ORDER BY msg_time, msg_index, id`，两个排序键在持久化时定死 → 乱序/交错入库对读序无害。
- `AppendMessages` 返回的自增 id 无人消费 → 异步化不破坏调用方。
- 现状 MySQL 插入失败仅记日志（"messages held in FS only"），存在仅存 FS 的历史孤儿消息 → 用户已决策不回填、直接丢弃。
- 两处既有 spec 漂移：turn-cost-tracking 的"同事务写 usage"（实现是独立调用）；session-management 的"orchestrator 失败不落库"（实现是部分落库）。

## Goals / Non-Goals

**Goals:**

- 每轮 turn 的持久化成本 = 一次 Redis pipeline（读缓存 + 入库队列双写），MySQL 写由后台 flusher 合并批量执行。
- 入库失败重试、不丢弃缓存数据（at-least-once）；配合幂等键达到实际 exactly-once。
- 整体移除 FS 温层的读写路径。
- 删除会话：先库后缓存，归档完整覆盖未入库消息。
- 闭合 Redis miss 回填时 DEL+RPUSH 抹掉未入库行的竞态。

**Non-Goals:**

- 不迁移/回填存量 FS 会话文件（孤儿消息接受丢失）。
- 不改 SSE 事件形状、历史分页 API、消息重建逻辑（`MessagesToAgentMessages`）。
- 不动 turn_usage 与 llmraw 机制。
- 不做跨主机队列/存储架构变更（buffer 与 MySQL 仍是既有单点部署形态）。

## Decisions

### D1 双 key 设计：读缓存与入库队列分离

`msgs:{session_id}`（读热层，保留 24h TTL，语义不变：过期 ≠ 丢数据，只意味着下次读走 MySQL 回填）与 `msgs:buffer`（全局 FIFO 队列，无 TTL，照搬 `llm_raw:buffer` 约定）在**同一个 Redis TxPipeline** 里双 RPUSH。

- 为什么不能合一：buffer 会被消费者移走，不能兼作读缓存；`msgs:{sid}` 是按 session 寻址的，全局队列无法按 session LRANGE。
- 为什么同 pipeline：一次 RTT，双写原子性最好（pipeline 失败 = 两边都没写，降级路径只需处理一种失败形态）。

队列元素复用消息的规范 JSON（与缓存元素同一 blob，decode 后即 `model.Message`）。

### D2 认领协议：LMOVE → processing → LREM（可靠队列）

flusher 每轮循环：逐条 `LMOVE msgs:buffer msgs:processing LEFT RIGHT` 原子认领（累计 ≤ 批阈值 N）→ 批量 `INSERT IGNORE` → 成功后逐条 `LREM msgs:processing -1 <raw>` 确认。进程启动时先把 `msgs:processing` 残留推回 `msgs:buffer` 头部再进入主循环（回收上一次崩溃的 in-flight 批）。

- 备选 A：照搬 llmraw 的 `LPOP → INSERT`。否决——pop 后 insert 失败，数据已离开 Redis，无法满足"重试不丢弃"。
- 备选 B：`LRANGE` peek → INSERT → `LPOP count`。否决——多进程各自 flusher 并发时，两个 flusher peek 到相同头部、各自 LPOP 会导致第二个 flusher 弹掉它从未插入的记录（丢数据）。
- 备选 C：单全局 flusher（Redis 锁选主）。否决——引入锁故障模式；LMOVE 天然多消费者安全，与 llmraw"每进程一个 flusher"的部署形态一致。
- 残余风险：INSERT 已提交、LREM 未执行时崩溃 → 回收后重复认领重复插入，由 D3 幂等键吸收。
- LREM 按 value 定位是 O(processing 长度)；processing 正常每轮清空、仅崩溃残留时非空，可接受。

### D3 幂等键：`client_msg_id` + UNIQUE + `INSERT IGNORE`

`model.Message` 增加 `ClientMsgID`，`SaveMessagesBatch` 持久化时为每条消息 mint UUID（新 migration 加 `client_msg_id CHAR(36)` 列 + UNIQUE 索引，遗留行 NULL——MySQL UNIQUE 索引允许多个 NULL，存量数据不受影响）。flusher 的批量 INSERT 改写为 `INSERT IGNORE`。

- 为什么不用自然键 `(session_id, msg_time, msg_index)` 做唯一索引：同 session 并发 turn 理论上可撞键，且把唯一性约束绑在业务排序键上语义脆弱；显式幂等键职责单一。
- 效果：重试、崩溃回收、降级直写（D4）三条路径共用同一幂等保障，实际 exactly-once。

### D4 Redis 写失败降级：同步直写 MySQL

`SaveMessagesBatch` 的双写 pipeline 失败时，退回同步直写 MySQL（`INSERT IGNORE`，同受 D3 保障），保留"消息必赢"的既有约定。现状（Redis 挂不影响写路径）语义得以延续：Redis 挂时从"跳过 Redis"变为"跳过队列、直写库"。

### D5 死信例外：FK 错误丢弃

重试策略为无限次 + 1s 退避，唯一例外是会话已删导致的 FK 错误（MySQL 1452）：这类记录永远不可能成功，继续重试会毒死队列 → 识别错误码后从 processing `LREM` 移除并记 ERROR（含记录摘要）。这是"永不丢弃"的唯一合法出口，与 D7 的删除前置 drain 互为兜底（drain 处理常规时序，死信处理并发边角）。

### D6 读 miss 先 drain：闭合回填竞态

`RecoverMessages` Redis miss 时，先执行一次**有界同步 drain**（复用 flusher 的 drain 原语，或确认 buffer+processing 为空），再读 MySQL 并 `SetMessages` 回填。否则回填的 DEL+RPUSH 会把"已 push 进新 `msgs:{sid}`、尚未入库"的行从缓存层抹掉，且该窗口从现状的毫秒级拉长到 flush 间隔级。miss 仅发生在 24h 空闲或 Redis 重启后，同步 drain 的代价可忽略。

### D7 删除会话三步：drain → 库 → 缓存

`DeleteSession`：① 同步 drain buffer+processing（该会话未入库消息先进 MySQL，归档因此完整）→ ② MySQL 单事务（归档镜像表 + 删 sessions 行，cascade 清 live 数据）→ ③ `DEL msgs:{sid}` / `session:{sid}`。第 ③ 步是行为变更：现状刻意不清 Redis 依赖 TTL，本次改为主动清除（先库后缓存，次序由用户指定；读路径对已删会话本就先经 MySQL 所有权校验，清缓存安全）。FS 会话文件删除步骤随 FS 层移除而消失。

### D8 flusher 归属、装配与参数

- 归属：agent / all 角色构造并启动（api 角色不构造，与 llmraw 一致）；每进程一个 goroutine，多进程安全（D2 的 LMOVE）。
- 参数：新顶层 config 块 `messages:`——`flush_interval`（默认 1s，比 llmraw 的 5s 短：历史接口对消息可见性更敏感）、`flush_batch_size`（默认 100）、错误退避 1s（常量，同 llmraw）。`0`/负值校验拒绝。
- `sessions.update_time` 从每轮同步 UPDATE 挪进 flusher：每批插入成功后对批内 distinct session_id 各刷一次；会话列表排序新鲜度滞后 ≤ flush 间隔，无感。刷新失败仅记日志。
- 优雅停机：复用 llmraw 形态——停止主循环后一次有界 final drain（超时上限），保证正常发布不丢尾部。

### D9 FS 层移除范围

`fs.Store` 收缩到 `EnsureUserDirs`（`CreateSession` 仍需要；`userSubDirs` 去掉 `sessions/`，只建 `workspace/`）。`RecoverMessages` 删 `tryFS` 分支与 `writeFSFromRaws`；`SaveMessagesBatch` 删 `appendToFS`；`FSStore` 接口与 `SessionDeps.FS` 相应收缩。存量 `data/{userID}/sessions/` 不迁移、代码不再触碰，运维自行清理。

### D10 部署前置：Redis AOF

buffer 成为唯一同步层后，Redis 裸奔（无 AOF）重启 = 丢 flush 间隔内的消息。列为部署要求（`appendonly yes`）；启动时尽力探测（`CONFIG GET appendonly`，无权限/失败时跳过），未开启则打 WARN 日志提示。不阻断启动。

### D11 turn_usage 时序

`SaveTurnUsage` 保持独立同步调用（在 Redis 入队返回后触发）→ usage 行可能先于消息行落地。可接受：usage 是观测数据，且删除会话时 cascade 一致；同时把 spec 里"同事务写 usage"的漂移修正为独立调用。

## Risks / Trade-offs

- [Redis 无 AOF 时 crash 丢 flush 间隔内的消息] → D10 部署要求 + 启动 WARN；文档写明风险窗口。
- [历史分页接口读己之写窗口 ≤ flush 间隔（默认 1s）] → 默认值取短；前端本就持有 SSE 流式内容，仅影响刷新/多端场景。
- [MySQL 长时间故障导致 buffer 无界增长] → 与 llmraw 同立场：重试不丢 + WARN 日志（带队列长度），operator 介入；不做自动丢弃。
- [删除接口因前置 drain 增加 ~一个 flush 周期的延迟] → drain 有界，DELETE 非交互热路径，可接受。
- [崩溃窗口内重复插入] → D3 `INSERT IGNORE` 吸收，实际 exactly-once。
- [回滚到旧版本时 buffer 内未入库消息丢失] → 回滚 runbook：先等 buffer 清空（日志/队列长度确认）再回滚；已入库消息为普通行，旧代码忽略 `client_msg_id` 列。
- [LREM 按 value 的 O(n) 扫描] → processing 列表正常为空，仅崩溃残留非空，规模 ≤ 批阈值 × 崩溃次数，可忽略。

## Migration Plan

1. **预检**：Redis 启用 AOF；确认可停机窗口内回滚预案。
2. **Migration 012**：`messages` 加 `client_msg_id CHAR(36) NULL` + UNIQUE 索引（存量库手动应用；旧代码写 NULL 不受影响，新代码 INSERT IGNORE 兼容旧表结构前的窗口期只是失去幂等保障）。
3. **部署新构建**：flusher 启动时自动回收 processing 残留（首次部署为空）；旧 FS 会话文件原地留存、不再读取。
4. **回滚**：等 buffer drain 完（日志确认空队列）→ 重新部署旧构建。`client_msg_id` 列对旧代码无感；Redis 里的 `msgs:buffer` 残留可由运维清空。
5. **收尾（可选）**：确认稳定后手工清理 `data/{userID}/sessions/` 目录。

## Open Questions

无阻塞项。实现期细节（migration 编号顺延、config 键名微调、flusher 包名 `internal/msgflush` 与否）由 tasks 落地时按仓库惯例定。
