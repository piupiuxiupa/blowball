## Context

现状链路：所有 StreamEvent（主+子 agent）→ `MergeEvents` 合并相邻同 agent token → `buildTurnMessages` 渲染为行 → Redis 双写（读缓存 + 全局队列）→ msgflush flusher 批量入 MySQL。1 秒中途增量持久化（`incrementalPersistEvery`）让 partial 内容可见，mid-turn compaction 依赖"先刷成持久行再压缩"。

## Goals / Non-Goals

- Goals：`messages` 行数从 O(events) 降到 O(rounds)；tool 交互结构化配对；子 agent 内容离开 `messages`；退役 msgflush chunk 管线；请求序列 byte-stable 保前缀缓存。
- Non-Goals：遗留 chunk 行迁移合并；`subagent_runs.messages_json` 改行存储（现有快照够用）；compaction 算法本身变更；SSE 实时流协议变更。

## Decisions

### D1 — 落库粒度：round 边界，而非 turn 结束

mid-turn compaction 对已持久化真实行有硬依赖（boundary cursor 必须指向 MySQL 行 id）。round 边界落库让 cursor 有行可指，同时崩溃最多丢当前 round、刷新可见延迟 ~1 round。这是 compaction 约束下能接受的最低写频率。

### D2 — tool_calls 独立表为 canonical，`(turn, round)` join

`tool_call_id` 负责 call↔result 同行配对（OpenAI wire format 原生语义）；对话流位置由 `(session_id, turn_id, round_no)` 序数确定——每轮内部结构固定（assistant → 该轮 tool calls → results），合并全确定，无时间戳排序歧义。不选 assistant 行内嵌 JSON：tool 结果是对话最大 payload，内嵌+索引表会双倍存储。

重建请求序列与现状 wire format 逐 byte 一致（现有逻辑本就把离散 tool_call 行 batch 成带 `tool_calls` 数组的 assistant 消息），迁移边界上前缀缓存连续命中。

### D3 — Redis 承载 in-flight，run event log 复用

流式 chunk 只留在 Redis。现有 run event log（Redis Stream，XREAD 重放、terminal 后 Retain）已是 in-flight 事件的载体，round 累积器在其上构建。round 完成后一个事务跨 `messages` + `tool_calls` 批写，幂等键复用确定性 client_msg_id / tool_call_id UK。

### D4 — 丢失边界

- **crash**：丢当前进行中 round（进程没了，无法 flush）；已完成 round 已落库。比现状更干净——不再有半截事件行进历史。
- **cancel / provider error**：turn 以非正常态结束时，把当前 round 已累积内容作为截断终稿行 flush（标注 interrupted），满足 interrupted-turn-persistence 既有语义。
- **用户消息**：send-time 写保留不变。

### D5 — 子 agent 分流

持久化层按事件 Meta 的 `agent_instance_id` 分流：非空 → 不进 `messages`（transcript 由 `subagent_runs` 快照服务，已有 API）；为空 → 终稿路径。SSE 实时流不受影响（hub 直达）。

### D6 — 遗留数据不迁移

新终稿行与旧 chunk 行按 event type 天然区分（`EventTypeToken` 等在新数据中绝迹）。`MessagesToAgentMessagesIndexed` 的合并逻辑保留为遗留适配路径，两种输入产出相同请求序列。

## Risks / Trade-offs

- 历史端点从单表分页变双表 merge——API 聚合形状需设计，但这是 DTO 层问题。
- compaction boundary 语义跨两表：cursor 在 `messages`，被压缩区间的 `tool_calls` 行不删（跳过即可），join 按 turn、boundary 也按 turn，语义对齐。
- Redis 持久化配置成为 in-flight round 的存活上限——crash 丢一轮已被接受，AOF 非硬依赖。
