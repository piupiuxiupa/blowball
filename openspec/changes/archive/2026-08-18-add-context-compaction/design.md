# design — add-context-compaction

## Context

当前模型上下文的构造路径：`MessageStreamHandler.SendMessage` → `MessageService.RecoverMessages`（Redis → drain → MySQL）→ `MessagesToAgentMessages`（重建为 agent 消息）→ 全量追加本次 user 消息 → `OrchestratorRunner.Handle`。两个无保护的增长点：

1. **turn 间**：每 turn 全量恢复全部历史，历史只增不减。
2. **turn 内**：`Confucius.Run` 的 `round` 切片随工具轮增长（`invoke_*` 的工具结果 = 子代理完整输出），单 turn 即可膨胀到爆窗。

持久化时序是关键约束：**turn 结束后才批量落库**（`SaveMessagesBatch`），turn 进行中的消息只在内存 `round` 与 orchestrator 收集的事件流里。显示路径（`GET /messages`）直查 `messages` 表，要求压缩后查询行为不变。

参考过 deepseek-harness 的 compaction 设计（分级流水线、结构化 checkpoint 模板、checkpoint 合并、KV cache 前缀重放），本设计按 blowball 的存储模型（append-only messages + 双表结构）简化落地。

## Goals / Non-Goals

**Goals:**

- 上下文达到 `80% × openai.max_context_tokens` 时自动压缩，会话可无限延续。
- mid-turn 触发时"暂停 → 压缩 → 继续"，单 turn 内也不爆窗。
- 显示路径零变更：`GET /messages`、Redis `msgs:` 缓存、`messages` 表均不感知压缩。
- 压缩失败永不阻断对话（降级为全量历史）。

**Non-Goals:**

- 不做无模型的工具结果剪枝（tool-result head/tail 截断）—— 后续可叠加的独立能力。
- 不做摘要模型单独配置（复用当前 agent 模型，用户已定）。
- 不做 KV cache 前缀重放优化（摘要请求独立构造，缓存命中交给 provider 前缀缓存自然发生）。
- 不做压缩事件的前端 SSE 通知（压缩完全静默）。
- 不处理"单个不可分单元本身超过窗口"的场景（如单条巨型消息），与 deepseek-harness 同样列为 out of scope。

## Decisions

### D1 计量：权威 usage，不用估算器

- **turn 内**：`Confucius.Run` 每轮 `StreamChat` 返回的 `resp.Usage.PromptTokens + CompletionTokens` 就是下一轮请求的真实上下文大小（含 system prompt、tools、全部历史），直接与 `0.8 × max_context_tokens` 比较，零估算误差。
- **turn 间（预防检查）**：`turn_usage.total_tokens` 是整 turn 各轮用量的**累加**（`total.Add(resp.Usage)`），远大于上下文大小，**不能使用**。新增 `turn_usage.context_tokens` 列记录每 turn **末轮**的 prompt+completion，turn 开始时读最新一行即可判断。
- **备选（否决）**：字符估算（chars/4 或 CJK 系数）—— 引入误差且需要维护 tokenizer 语义；blowball 事后有真值，没有必要。
- 首个 turn / 无历史 usage 时（新会话）不触发，天然安全。

### D2 触发点：两处，共用一个压缩服务

1. **轮间检查（mid-turn，主路径）**：`Confucius.Run` 循环内，工具结果 append 之后、下一轮 LLM 请求之前。超阈值 → 暂停循环执行压缩 → 用压缩后的上下文重写 `round` → 继续。
2. **turn 开始检查（预防，省钱路径）**：`SendMessage` 在 recover 之后、orchestrate 之前，读 `turn_usage.context_tokens` 最新值。超阈值 → 先压缩再发送，省掉一次注定要压缩的全窗口请求。

两处都调 `CompactionService.Compact(ctx, sessionID, userID, trigger)`。触发条件额外要求**中间部分非空**（可压内容 > 0），否则跳过。

### D3 mid-turn 流程：先落库再压缩（flush-first）

用户已定：触发压缩时先把已有数据按现有逻辑保存，再压缩。这把"turn 内消息未持久化、边界 id 不存在"的问题消掉了 —— 压缩永远面对完整落库的历史，边界永远是真实 message 游标。

```
轮间检查超阈值
  ① flush：把 orchestrator 已收集的本 turn 事件（user 消息 + 事件流）
     经 MergeEvents → MessageFromEvent → SaveMessagesBatch 落库（现有路径复用）
  ② 压缩：Recover（此刻含刚 flush 的消息）→ Reconstruct → 选范围 → 摘要 → 写记录
  ③ 重写内存：round = [首条] + [<compacted-summary> 摘要] + [尾部]
  ④ 继续循环；turn 结束时只补存 flush 之后的新事件
```

事件流在 orchestrator（`Handle` 边流边收集），不在 Confucius 手里 —— 通过**轮间钩子**注入：orchestrator 构造 `RoundHook(ctx, lastUsage, collectedEvents)` 传给 Confucius 循环，钩子内执行上述 ①②③。压缩逻辑全部在 `CompactionService`，Confucius 只持有钩子接口（保持 agent 层不依赖 service 层的具体实现）。

### D4 落库幂等：确定性 client_msg_id

现状 `UserMessage` / `MessageFromEvent` 不设 `ClientMsgID`，turn 结束保存只跑一次所以无需幂等。mid-turn flush 引入第二次保存后：

- **MySQL**：基础设施已就位 —— `client_msg_id` 有 UNIQUE 索引 + `INSERT IGNORE`。为本 turn 全部消息生成确定性 id：`{trace_id}:{msg_index}`（msg_index 在 turn 内唯一）。flush 与 turn 结束保存按同一规则生成 → 自动去重。存量历史行为 NULL，不受影响。
- **Redis**：`RPUSH` 不去重。turn 结束的批量保存若发生过 mid-turn flush，**只推送 flush 之后的新增消息**到 `AppendMessagesDual`（按 msg_index 切分），避免 `msgs:{sid}` 出现前缀重复。
- **备选（否决）**：turn 结束时 `SetMessages` 整表重建 —— 会把 write-behind 队列语义搅浑（重建绕过 ingest 队列），增量推送改动面更小。

### D5 压缩范围：agent 消息层切片，配对原子

切片在 `MessagesToAgentMessages` **之后**的 agent.Message 序列上做（不是持久化行上）：

- 首条 = 序列第一条 user 消息（原始任务），永远保留。
- 尾部 = 末尾 5 条 agent 消息（配对后的完整对话单元），保留。
- 中间 = 其余全部，进入摘要。
- **边界约束**：尾部切点不得落在"assistant(tool_calls) 与其 tool 消息"之间 —— 按配对单元整体划分（`MessagesToAgentMessages` 的输出天然按配对单元分组，末尾回退到配对边界即可）。理由：重建对未配对 tool_call 的容错是**静默丢弃**（`message_reconstruct.go` 的 `continue` 分支），切开配对不会崩但会丢内容。
- 重复压缩：新摘要输入 = 旧摘要（来自最新压缩记录）+ 旧边界到新边界的内容，**合并成单一摘要**（checkpoint-merge，不滚雪球）。若无旧记录则直接摘要中间部分。
- 范围选择后回写边界游标 = 尾部第一条消息对应的**持久化行**的复合游标 `(msg_time, msg_index, id)`（在 flush 后的行列表里定位）。

### D6 摘要生成：当前模型 + 结构化模板

- 复用当前 agent（Confucius）配置的模型，经现有 `OpenAIClient`（非流式语义收集，走 `StreamChat` 丢弃 token 回调或加一个便捷封装）。调用自动进入 `llm_raw_log`（raw-capture），摘要的可观测免费获得。
- 指令采用结构化 checkpoint 模板（8 段：Primary Request and Intent / Key Technical Concepts / Files and Code / Errors and Fixes / Pending Jobs / Current Work / Next Step / Critical Context），要求保留精确路径、命令、错误串、数字、签名；已有 `<compacted-summary>` 时合并不堆叠。
- 只取返回 text；丢弃 reasoning 与 tool_calls（防私有推理泄漏 / 孤儿调用）。
- 压缩调用自身计入 turn usage 吗？**不计入** —— 它是运维性调用，与 deepseek-harness 的 `purpose=compaction` 归因思路一致；usage 只记入压缩记录的 `summary_prompt_tokens` / `summary_completion_tokens` 观测列。
- 摘要为空/失败：本 turn 放弃压缩，记 WARN 继续原上下文（失败降级原则，见 D8）。

### D7 存储：append-only 压缩表 + sessions 标记 + Redis 热缓存

`migrations/012_context_compaction.sql`（列类型对齐 010/011 的实际写法）：

```sql
ALTER TABLE sessions
  ADD COLUMN context_compacted TINYINT(1) NOT NULL DEFAULT 0;

ALTER TABLE turn_usage
  ADD COLUMN context_tokens INT NOT NULL DEFAULT 0;

CREATE TABLE context_compactions (
  id                        BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  session_id                VARCHAR(64)  NOT NULL,
  user_id                   VARCHAR(64)  NOT NULL,
  trace_id                  VARCHAR(64)  NOT NULL,
  trigger_kind              VARCHAR(16)  NOT NULL COMMENT 'mid_turn | turn_start',
  trigger_tokens            INT          NOT NULL,
  content                   MEDIUMTEXT   NOT NULL COMMENT '裸摘要, 框架在拼接时包装',
  boundary_msg_time         DATETIME(3)  NOT NULL,
  boundary_msg_index        INT          NOT NULL,
  boundary_msg_id           BIGINT       NOT NULL COMMENT '复合游标指向最后一条被覆盖消息',
  shadowed_tokens           INT          NOT NULL,
  summary_model             VARCHAR(128) NOT NULL,
  summary_prompt_tokens     INT          NOT NULL DEFAULT 0,
  summary_completion_tokens INT          NOT NULL DEFAULT 0,
  create_time               DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_ctx_comp_session (session_id, id),
  CONSTRAINT fk_ctx_comp_session FOREIGN KEY (session_id)
    REFERENCES sessions (session_id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
```

- **append-only，latest-wins**：每次压缩插一行，拼接取 `(session_id, id)` 最大行。不用 `UNIQUE(session_id)`+UPDATE —— 保留审计轨迹，重跑幂等。历史行无热路径消费者，不进缓存。
- **边界用三列复合游标**而非单 id：对齐 `messages` 的全局排序 `(msg_time, msg_index, id)`，并发交错插行时 id 与逻辑顺序可能脱钩，复合游标与现有分页语义一致。
- **FK CASCADE、无镜像表**：随 `turn_usage` 先例，随会话删除级联清理，不进 008 删除归档（会话私有业务数据；深度观测由 `llm_raw_log` 中的摘要调用兜底）。
- **Redis `compaction:{session_id}`**：值为最新行的 JSON 投影（`id` / `content` / `boundary` / `create_time`），TTL 同 `msgs:` 键，新压缩整键覆盖。读路径 GET → miss → MySQL 最新行 → 回填（与 `RecoverMessages` 同款三段式）。会话删除时随 `DelSessionCache` 一并清理。
- **sessions 标记**：`SendMessage` 每次都 `GetSessionByID`（标记零额外查询成本）。置 1 后永不回 0。

### D8 写序与失败降级

**写序（崩溃一致性）**：`① INSERT context_compactions → ② SET compaction:{sid} → ③ UPDATE sessions.context_compacted = 1`。任意一步后崩溃：记录成为孤儿行（flag=0），下次压缩插新行 latest-wins，无害。反序会造成 flag=1 查无记录。

**降级原则（压缩是尽力而为，永不阻断对话）**：

- 拼接时 flag=1 但 Redis/MySQL 均无记录 → 降级全量历史。
- 摘要调用失败 / 返回空 → 放弃本次压缩，WARN 后继续原上下文。
- mid-turn flush 失败 → 放弃本次压缩（压缩对 flush 成功有硬依赖），WARN 继续。
- 压缩记录写入部分失败（表成功、Redis 失败）→ 继续（Redis 会由 miss 回填自愈）。
- 唯一硬失败：`max_context_tokens` 配置了非法值（非正数）→ 配置加载期报错（fail fast，与 `reasoning_effort` 校验先例一致）。

### D9 拼接路径（后续发送）

`SendMessage` 在 `MessagesToAgentMessages` 之后：

```
sessions.context_compacted = 1?
  └─ 是 → 读 compaction:{sid}（Redis → MySQL 回填）
       → 在重建消息序列中按边界游标定位切点
       → 模型上下文 = [首条 user 消息] + [摘要 user 消息(content = 前导语 +
          <compacted-summary>…</compacted-summary>)] + [切点之后全部消息]
  └─ 否 → 现状全量
然后追加本次 user 消息，进入 orchestrator。
```

摘要消息 role=user（模型对 user 角色的既定背景遵从性最好，deepseek-harness 同款选择）。前导语固定：*"This is an automatically generated checkpoint condensing an earlier span of the conversation..."*（中文会话不例外 —— 摘要指令与框架用英文工程散文，与 deepseek-harness 模板一致）。

## Risks / Trade-offs

- [摘要调用本身可能超窗] 中间部分接近窗口时，摘要请求（输入=中间内容+指令）也会 400。**缓解**：接受（与 deepseek-harness 同样的已知限制）；80% 阈值意味着正常触发时中间 ≤80%，一次摘要多数可行。极端场景留给后续的分段摘要（Non-Goal）。
- [80% 阈值下 turn 内仍可能爆窗] 阈值检查在轮间，最后一轮 LLM 响应自身 + 新工具结果可能把下一次请求推过 100%。**缓解**：触发即压缩后重写 round，后续轮从低位重启；残余窗口由 `max_tokens` 输出上限约束，实际风险低。
- [压缩期间用户感知延迟] mid-turn 压缩在 SSE 流中表现为一段停顿（摘要调用耗时）。**缓解**：接受（压缩低频发生）；后续可加可忽略的 SSE 事件。
- [首次全窗口请求成本] turn 开始预防检查依赖上一 turn 的 `context_tokens`；会话恢复/升级场景该列为 0，第一 turn 不做预防，靠轮间检查兜底（最多多付一次全窗口请求）。**缓解**：接受 —— 正确性不受影响。
- [并发 turn] 同会话并发发送消息本就无锁（现状），mid-turn flush 的确定性 id 使 MySQL 侧幂等，Redis `msgs:` 增量推送按 msg_index 切分在并发下可能交错 —— **缓解**：与现状同等风险，不引入新失效模式；会话级串行化为后续独立工作。
- [阈值固定 80%] 不可配置。**缓解**：常量集中定义，后续要开放为配置是一行改动。

## Migration Plan

1. 应用 `012_context_compaction.sql`（docker compose 首次初始化自动执行；存量库手动应用 —— 与 010/011 同流程）。
2. 新表/新列全部带默认值，`context_compacted=0` / `context_tokens=0` 使旧行为完全不变：**未配置 `openai.max_context_tokens` 时压缩整体不激活**（零值 = 关闭，兼容现网）。
3. 回滚：配置去掉 `max_context_tokens` 即回到现状；表结构无需回滚（新表/新列不被旧代码读取）。

## Open Questions

（无 —— 触发时机、配置粒度、摘要模型、表结构、`turn_usage.context_tokens` 采纳与否均已在探索阶段与用户确认。）
