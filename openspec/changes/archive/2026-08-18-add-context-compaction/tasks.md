# tasks — add-context-compaction

## 1. 配置与迁移（地基）

- [x] 1.1 `internal/config/config.go`：`OpenAIConfig` 新增 `MaxContextTokens int`（yaml `max_context_tokens`），零值=关闭；load 校验非零时必须为正整数，否则报错
- [x] 1.2 `config.example.yaml` 增加 `openai.max_context_tokens` 注释示例（说明零值关闭、80% 触发阈值）
- [x] 1.3 `migrations/012_context_compaction.sql`：新表 `context_compactions`（DDL 见 design D7）+ `sessions.context_compacted TINYINT(1) DEFAULT 0` + `turn_usage.context_tokens INT DEFAULT 0`（实际落为 `013_context_compaction.sql`——`012` 已被 client_msg_id 占用）
- [x] 1.4 `internal/model/` 新增 `ContextCompaction` 模型（含边界复合游标三列与观测列）

## 2. 存储层

- [x] 2.1 `internal/store/mysql/compaction.go`：`InsertCompaction` / `LatestCompaction(sessionID)`（`(nil, nil)` 缺省先例）；`UpdateSessionCompacted(sessionID)` 置标记；`turn_usage` 写入路径带上 `context_tokens` 列
- [x] 2.2 `internal/store/redis/compaction.go`：`compaction:{session_id}` 键的 Get/Set/Del，TTL 同 msgs 键族；`DelSessionCache` 顺带清理该键
- [x] 2.3 `internal/service/deps.go`：`SessionDeps` 扩充 compaction 存储接口（沿用小接口模式，测试可替换 fake）

## 3. CompactionService（压缩核心）

- [x] 3.1 新建 `internal/service/compaction.go`：`CompactionService` 骨架 —— 依赖注入（MySQL/Redis deps、LLM client、confucius 配置）、`Compact(ctx, sessionID, userID, triggerKind)` 入口、`contextTokens` 阈值常量（0.8）
- [x] 3.2 范围选择：reconstruct 后的 agent 消息序列上切片（首条 user 排除、末尾 5 条保留、按配对单元回退边界），映射回持久化行复合游标；中间为空时返回“无需压缩”
- [x] 3.3 摘要生成：结构化 checkpoint 指令模板（8 段 + 合并规则），复用当前模型经 `OpenAIClient` 调用，只取 text（丢弃 reasoning/tool_calls）；已压缩会话将旧摘要并入输入
- [x] 3.4 写序落库：`InsertCompaction → Redis Set → UpdateSessionCompacted`，全部成功方算压缩完成；各步失败按 design D8 降级（WARN，不阻断）
- [x] 3.5 降级读取：`LatestCompactionRecord(ctx, sessionID)` —— Redis → miss → MySQL → 回填；flag=1 且双 miss 时返回 nil（调用方降级全量）

## 4. 拼接路径（turn 间）

- [x] 4.1 `internal/handler/message_stream.go`：`SendMessage` 在 `MessagesToAgentMessages` 之后按 `sess.ContextCompacted` 调拼接 —— 首条 + 框架包装摘要（user role）+ 边界游标后消息 + 本次 user 消息；记录缺失时全量降级
- [x] 4.2 `turn_usage.context_tokens` 记录：turn 结束持久化时取末轮 LLM usage 的 prompt+completion 写入（`buildTurnUsage` 扩展）——经 done 事件 `usage.meta.context_tokens` 传递（agent 层 `TurnBreakdown.LastRoundContextTokens`）
- [x] 4.3 turn 开始预防检查：`SendMessage` 在 orchestrate 前读最新 `turn_usage.context_tokens`，超阈值且中间非空则先调 `Compact(trigger=turn_start)`

## 5. mid-turn 流程（turn 内）

- [x] 5.1 `internal/handler/event_mapper.go`：`UserMessage` / `MessageFromEvent` 生成确定性 `ClientMsgID`（`{trace_id}:{msg_index}`）——迁移 013 顺带将 `client_msg_id` 列放宽为 CHAR(64)（UUID trace_id 已占满 36 字符）
- [x] 5.2 mid-turn flush：orchestrator 收集事件 → `MergeEvents` → `MessageFromEvent` → `SaveMessagesBatch` 复用现有路径；flush 后 turn 结束保存只推增量（Redis dual-write 按 msg_index 切分）——快照经 `TurnEventTap` 与事件收集协程同步（adapter 优先排空 hub 通道），保证合并序号在 flush 与 turn 结束之间稳定
- [x] 5.3 轮间钩子：`internal/agent/` 定义 `RoundHook` 接口（携带最新 usage；事件流由钩子闭包经 `TurnEventTap` 捕获），`Confucius.Run` 在工具结果 append 后、下一轮请求前调用；`internal/agent/orchestrator.go` 接线实现 —— 钩子内 flush → `Compact(trigger=mid_turn)` → 重写 `round`（首条+摘要+尾部）
- [x] 5.4 mid-turn 压缩后 `round` 重写：摘要消息按拼接同款框架包装，保证 turn 内外上下文形态一致

## 6. 测试

- [x] 6.1 `internal/config/`：`max_context_tokens` 校验单测（0 通过、正数通过、负数/非法拒绝）
- [x] 6.2 `internal/service/compaction_test.go`：范围选择（首条/尾部/配对边界回退/中间空）、写序、降级路径（记录缺失、摘要失败、flush 失败）
- [x] 6.3 `internal/handler/`：拼接路径单测（flag=1 拼接、flag=0 现状、记录缺失降级）；确定性 client_msg_id 生成与幂等（同 id 不重复插入）
- [x] 6.4 `test/integration/`：假 LLM 下全流程 —— 阈值触发压缩、压缩记录落库、后续 turn 拼接、mid-turn flush 与 turn 结束保存无重复行、`GET /messages` 返回不含摘要且行数完整
- [x] 6.5 重复压缩集成场景：二次触发合并旧摘要，latest-wins，`msgs:` 缓存无重复

## 7. 文档

- [x] 7.1 CLAUDE.md：架构章节补压缩段落（触发、表结构、拼接路径、mid-turn 流程、配置）
- [x] 7.2 检查 `api/openapi.yaml` 无需变更（无新端点、无契约改动）并确认 `GET /messages` schema 未被触碰（唯一改动：`client_msg_id` 描述去掉 `format: uuid` —— 确定性 id 使该标注失实；类型仍为 nullable string，无契约变化）
