## Context

`messages` 表当前每个 turn 写 2 行（user + assistant），但 assistant 行的 `content` 是 `orchestratorAdapter` 在 drain innerHub 时把 Confuse 的 token delta **现场拼接**而成（`internal/handler/ports.go:67-69` 的 `content += e.Content`）。这等于把模型真实的事件流（token 分片、tool_call、子 agent 输出、agent lifecycle marker）压扁成一段文本，丢失了原始结构。

更糟的是排查中还发现两个 bug：
- `MsgIndex` 字段从 0 到 N 全工程无任何赋值代码，所有行 msg_index 都是 0，`ORDER BY msg_index ASC` 完全失效，目前靠 PK 顺序碰巧读对。
- 用户消息的 `Agent` 列被错填成 `model.AgentConfuse`（`handler/session.go:119`），与字段语义（产生消息的 agent）不符。

本设计把 messages 改造成"事件流日志"：用户消息一行（`event_type='message'`），assistant 一轮产生的每个 `StreamEvent` 各一行，按 `(msg_time, msg_index)` 严格有序。

## Goals / Non-Goals

**Goals:**
- messages 表保留模型原始事件流，可事后回放一个 turn 内 token / tool_call / lifecycle 的完整序列
- 修正 MsgIndex / Agent 字段的语义和赋值，让排序真正起作用
- 降低 assistant 路径的写入次数（一次批量 INSERT 替代 N 次）
- 失败路径不污染消息历史

**Non-Goals:**
- 不改 HTTP SSE 协议——前端看到的流式事件序列与现在完全一致
- 不存 `done` 事件的 usage 数据（usage 是流终止元数据，不是 chat 内容；后续如需统计走另外的表）
- 不引入 `turn_id` 列——靠 `msg_time` 区分不同 turn
- 不动 `confuse.go` / `chongzhi.go` / `liang.go` 的内部循环逻辑，只改外部入库装配

## Decisions

### Decision 1: schema 新增 `event_type` 列而非扩 `role`

**选择**：新增 `event_type VARCHAR(16) NOT NULL`，取值 `message | token | tool_call | agent_start | agent_end | agent_error`。

**备选 A**：扩 `role` 列把 marker 塞进去（`role=agent_start`）。否决理由：role 是 OpenAI chat 语义（user/assistant/tool），硬塞 marker 破坏了从 DB 反向构造 OpenAI messages 的能力（虽然现在没这么做，但保留这个 option 成本低）。

**备选 B**：新增 `meta JSON` 列存所有事件元数据（args、error_code 等）。否决理由：tool_call 的 args 走 content JSON 已足够（content 列注释本就允许 JSON），其他 marker 事件没有 meta 需要存。

### Decision 2: 用户消息 `agent='user'`，`event_type='message'`

用户消息不是流式事件，但需要跟 assistant 事件一起按时间排序。给它分配 `event_type='message'` 作为语义标记，可查询、可区分。schema 注释需要同步更新（之前写的是 `Confuse | Chongzhi | Liang`）。

### Decision 3: `role` 改为可空，`msg_time` 升到毫秒精度

`agent_start`/`agent_end` 这些 marker 没有 OpenAI role 语义，强行填值会误导。改 NULL 后读侧用 `role IS NOT NULL` 过滤 marker 即可。

`msg_time` 从 `TIMESTAMP`（秒）升到 `TIMESTAMP(3)`（毫秒）：batch INSERT 内所有行共享一个时间戳，秒精度下相邻 turn 极易撞同秒，必须靠 msg_index 作为 tiebreaker；毫秒精度大幅降低撞时间概率，但仍是"理论上可能"，所以排序键仍是 `(msg_time, msg_index)` 组合而非单 msg_time。

### Decision 4: 排序键 `(msg_time, msg_index)`，不引入 `turn_id`

**选择**：不加 `turn_id` 列，读时 `ORDER BY msg_time ASC, msg_index ASC`。

**备选**：加 `turn_id` 列后 `ORDER BY turn_id, msg_index` 更显式。否决理由：用户 msg 的 msg_time（同步写）严格早于 batch 的 msg_time（goroutine 异步写），靠时间戳区分 turn 已足够；加 turn_id 多一列、多一处赋值点，收益不抵成本。

**约束**：msg_index 在每个 turn 内单调（用户消息=0，assistant batch 从 1 递增）。**不跨 turn 单调**。这意味着 `ORDER BY msg_index` 单独使用无意义，必须配合 msg_time。

### Decision 5: `OrchestratorRunner.Handle` 返回 `([]StreamEvent, error)`

**选择**：把接口签名从 `(string, error)` 改成 `([]StreamEvent, error)`，adapter 内部把 `content += e.Content` 替换为 `events = append(events, e)`。

**备选**：保持接口不变，handler 另起一个 goroutine 从 hub 双读（一个走 SSE、一个走 collector）。否决理由：hub 是单消费者 channel，双读会偷事件；adapter 已经在 drain innerHub，复用这个 goroutine 零成本。

### Decision 6: 仅成功路径批量入库

`res.err != nil` 时直接 return，丢弃 collected events。理由：失败时的事件流可能不完整（agent_error 后是否还有 agent_end 不确定），写入会污染历史。用户消息已经在 orchestrator 启动前 sync 写入，失败也不丢用户输入。

### Decision 7: 用户消息保持同步写入

用户消息在 orchestrator 启动前 sync SaveMessage。理由：客户端可能 mid-stream 断连，sync 写保证用户输入永不丢；如果改成走批量 goroutine，断连时 detached ctx 仍能完成，但失去"sync 即可见"的语义。assistant 事件可以异步是因为它们是"衍生内容"，丢了用户可以重发。

### Decision 8: `done` 事件不入库

`done` 携带的 usage 是一次请求的统计元数据，不是 chat 内容。入库语义不一致。如未来要做 token 用量分析，单独建 usage 表更合适。

### Decision 9: tool_call args 存 content 列 JSON

格式 `{"name":"<tool_name>","args":<args_json>}`，content 列原本就允许 JSON（schema 注释 "text or JSON for tool calls"）。不加 args 列、不加 meta 列。

## Risks / Trade-offs

- **[Risk] msg_time 撞同毫秒**：batch 内所有行 msg_time 完全相同；理论上不同 turn 也可能撞。→ 缓解：msg_index 作为 tiebreaker；msg_index 在 batch 内严格从 1 递增，user msg=0，所以 (msg_time, msg_index) 在一个 turn 内绝对有序，跨 turn 靠 msg_time 区分。极端情况下两个 turn 撞同毫秒概率 < 1/1000，且即便撞了，PK id 仍可作为最终仲裁（ListMessages SQL 不显式 tiebreaker，MySQL 默认按 PK 顺序稳定排序）。

- **[Risk] 失败时丢失事件流**：orchestrator 出错 → 整批丢弃。→ 已接受（Decision 6）。如果未来需要失败审计，可加 audit log。

- **[Risk] 批量入库 goroutine 比响应返回晚**：客户端 SSE 流结束、HTTP 响应关闭后，goroutine 可能还在写 MySQL。→ 缓解：用 `trace.WithContext(context.Background(), tid)` 派生 detached ctx，goroutine 不依赖请求 ctx；goroutine 内部 panic recover 防止进程崩溃；前端如立即翻历史可能短暂看不到本轮 assistant 事件（一致性窗口 < 100ms 通常）。

- **[Risk] 接口签名 BREAKING**：`OrchestratorRunner.Handle` 返回类型变更，影响 handler 测试中的 stub。→ 缓解：handler 测试 stub 改返回 `[]StreamEvent`，工作量小；不影响生产代码（只有一个生产实现 `orchestratorAdapter`）。

- **[Trade-off] marker 事件占行**：每 turn 多几行 content 为空的 marker。→ 已接受。MEDIUMTEXT NOT NULL 允许空串，存储成本可忽略。

- **[Trade-off] 子 agent token 让 batch 行数膨胀**：Chongzhi/Liang 被调用时 token 数可能远超 Confuse 本身。→ 已接受。这正是"事件流"的语义需要，前端可据此精确回放 UI。

## Migration Plan

**Migration 005_messages_event_type.sql**：
```sql
ALTER TABLE messages
  ADD COLUMN event_type VARCHAR(16) NOT NULL DEFAULT 'token'
    COMMENT 'message | token | tool_call | agent_start | agent_end | agent_error'
    AFTER role,
  MODIFY role VARCHAR(16) NULL
    COMMENT 'OpenAI role (user/assistant/tool); NULL for marker events',
  MODIFY msg_time TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3);
```

**存量数据兼容**：
- 现有行 `event_type` 默认 `'token'`，但现有 role=user 的行应该改成 `event_type='message'`。建议在 migration 里加一条 UPDATE：
  ```sql
  UPDATE messages SET event_type='message' WHERE role='user';
  ```
- 现有 role=assistant 的行（拼接文本）保留 `event_type='token'`，视为一条聚合 token——虽然语义不严格（不是单个 delta），但兼容历史，避免数据丢失。
- msg_index 升级不影响现有数据（值不变，只是有了真实赋值）。

**回滚**：
```sql
ALTER TABLE messages
  DROP COLUMN event_type,
  MODIFY role VARCHAR(16) NOT NULL,
  MODIFY msg_time TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP;
```
回滚后新代码不兼容（找不到 event_type 列），需先回滚代码再回滚 schema。

**部署顺序**：
1. 先发 migration（加列、放宽约束）
2. 再发新代码（写 event_type、批量入库）
3. 老代码读老数据：兼容（默认 event_type='token'、role 已有值）
4. 新代码读老数据：兼容（同上）

## Open Questions

（无——四个关键决策已经在 explore 阶段与用户敲定）
