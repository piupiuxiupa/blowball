# Design: send-time-user-message-persistence

## 决策 1：写入位置——claim / title / InitMeta 之后，turn goroutine 启动之前

后于所有拒绝路径，前于 turn 启动。关键约束是**不能更早**：

- 400（校验）/ 404（会话不存在）/ 409（SESSION_BUSY）路径必须零写入——全部在 claim 之前 return；
- turn-start preventive compaction 与 stitched-context 组装读的是 durable 历史（`RecoverDurable`），在 claim 之前执行——若 user row 先落库，恢复出的历史会包含本 turn 用户消息，而 LLM 上下文又显式 `append(agentMsgs, user)`——**同一条消息进两次上下文**。代码顺序上 claim 晚于 stitching 块，写入点跟在 claim 后天然满足；若未来重排 SendMessage 前半段，须保持"user-row 写入晚于本 turn 的全部历史读取"这一不变量；
- title cadence 的序号数的是 `prior`（更早捕获的局部变量），与写入先后无关。

**同步调用，不做 fire-and-forget**：写入在 handler goroutine 上、turn goroutine spawn 之前完成，spawn 的 happens-before 边保证 round hook（mid-turn flush）与终局保存无需额外同步即可见到置位后的标志（`turnFlushState` 的互斥锁本就覆盖）。代价：claim 之后、SSE 首字节之前多一次 Redis pipeline（ms 级）；Redis 故障走 `SaveMessagesBatch` 内部既有的直写 MySQL 降级，与所有现存调用点同约定。

## 决策 2：`userPersisted` 独立于 `flushed` 计数

`buildTurnMessages(from)` 的 `from == 0` 目前同时决定"含 user row"与"含事件前缀"两件事。发送时写入后出现新中间态——"user 已持久化但事件 from=0"——故拆成两个维度：`turnFlushState` 增加 `userPersisted bool`（mark / check 走既有互斥锁），user-row 包含条件改为 `from == 0 && !userPersisted`。调用方（round hook 与 `persistEvents`）显式传入该 bool，`turnPersistInfo` 值语义不变。

核心不变量：**user row 在 `msgs:{sid}` 恰好 append 一次**。Redis list 无去重语义（MySQL 的 `uk_messages_client_msg_id` + INSERT IGNORE 只是 MySQL 侧兜底），任何路径重复 append 都会让 `RecoverMessages` / 历史读取读到重复行，直到 24h TTL 过期。三条渲染路径（发送时、mid-turn flush、turn 终局）共享同一标志。

## 决策 3：失败语义与 at-least-once

标志仅在 `SaveMessagesBatch` 返回 nil 时置位：

- 正常：Redis 双写成功 → 置位；
- 降级：Redis pipeline 失败 → 内部同步直写 MySQL（无论直写成败都返回 nil）→ 置位。两层全挂 = 行丢失但仍置位——与今天终局路径的 lost-and-logged 约定一致，不新增行为；
- error 返回（幂等键 mint / marshal 失败；user row 预盖 `{trace_id}:0` 后 mint 不触发、marshal 是纯结构体序列化，实际不可达）→ 不置位 → 终局批次照旧含 user row，确定性 id 在 MySQL 收敛。此路径理论上会在 `msgs:{sid}` 留一次重复 append，接受（不可达路径不为其加复杂度）。

## 决策 4：mid-turn compaction 的可见性变化是良性的

变更后，round hook 的 `RecoverDurable` 从 turn 第一轮起就能看到本 turn user row。这与今天"mid-turn flush 已把 user row 落库后"的 recover 语义完全一致——round 重写路径本就按"user row 恰好一份"处理。flush 渲染改为纯事件后缀（`from = flushed.count()`，user row 被 `userPersisted` 排除），recover 端状态与今天等价。顺带消除 Why 中的问题 3：所有 turn 的 user row 统一在发送时可见，不再依赖 compaction 是否触发。

## 决策 5：残留窗口——历史端点 ≤ flush_interval

读路径是两条，勿混：**`RecoverMessages`（内部恢复，Redis-first）** 对发送时写入**零窗口**——`RPUSH msgs:{sid}` 在请求内同步完成，下次 `SendMessage` 恢复上下文即命中；**历史分页端点 `GET /messages`（前端刷新所用）直读 MySQL**（`GetSessionMessages` → `ListMessagesPaged`），绕过 Redis 是游标分页的结构性约束——复合游标 `(msg_time, msg_index, id)` 无法跨 Redis/MySQL 边界续页（缓存行 `id=0`，flush 后变真值，游标失配）。因此该端点上发送时写入经 flusher 落 MySQL 有 ≤ `messages.flush_interval`（默认 1s）延迟——发送后 1s 内刷新仍可能看不到 user row。与既有 read-your-writes 窗口同类（今天的终局写入同样有此窗口，spec 已接受），不为它把同步 drain 加进历史端点。

## 决策 6：前端零改动的推演（reconcile 不受影响）

`reconcileTurnHistory` 的收敛条件是 `after > baseline`：

- **发送路径**：baseline 含 onMutate 写入的乐观用户消息（N+1）；终局后 after = N+1+k——**等的是 assistant 行**（k≥1 才 break），user row 提前落库不改变条件两端；
- **attach 路径**：baseline = 刷新后历史条数（变更后已含 user row，即 N+1）；终局后 after 同比 +k——同样等 assistant 行。

两条路径都以"assistant 行出现"为收敛信号，本变更不触碰。刷新重进场景（问题 1）由"历史读取含 user row + attach 重放 agent 事件"直接覆盖，前端渲染管线无需感知。

## 决策 7：明确不做

- 不在 SSE / 事件日志引入 `message` 事件（sentinel 约定保持）：事件合成方案只能修显示，修不了崩溃丢失，还要动前端与 run 日志语义；
- 不把 assistant 事件也提前持久化：6/17 延迟优化的核心即在此，保持终局批量（mid-turn flush 仍是 compaction 触发的既有例外）；
- 不为历史端点加同步 drain（决策 5）。
