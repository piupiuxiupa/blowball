## 1. 后端：每次 invoke 新建子 agent 实例（方案 B）

- [x] 1.1 将 `Confucius.subAgents` 的值类型从 `Agent` 改为按调用构造的工厂（`func() (Agent, error)` 或封装 cfg/client/registry 的 blueprint），`NewConfucius` 签名与校验相应调整
- [x] 1.2 `orchestratorFactory.Build` 改为向 Confucius 交付 Chongzhi/Liang 的构造函数；per-turn `mcp.Manager` 与 `TurnCloser` 保持整 turn 共享、被构造函数捕获
- [x] 1.3 `dispatchSubAgent` 每次调用现 build 实例、用完即弃；重试循环复用同一实例（同一次调用的重试语义不变）
- [x] 1.4 单测：并发同名调用下 `executedToolThisRun`/`hitCapThisRun` 互不串读（X1 已执行工具不影响 X2 的重试资格；仅 X1 命中 cap 时只有 X1 传播 `round_capped`）

## 2. 后端：事件流 run 身份（方案 C — hub 切面）

- [x] 2.1 新建 `runTaggerHub`（内嵌 `*Hub`，仅覆写 `Send/SendCtx`，发送前向 `Meta` 注入 `parent_tool_call_id`，已有值不覆盖），单测断言全部子 agent 事件带标注且生命周期方法透传
- [x] 2.2 `dispatchOne` 在进入子 agent 分支前用 `tc.ID` 包装 hub 传入 `sub.Run`；registry 工具分支不经包装
- [x] 2.3 单测：Confucius 自身事件与用户消息不携带 `parent_tool_call_id`；`meta` 为 nil 的事件被正确初始化并标注

## 3. 持久化与契约

- [x] 3.1 `model.Message` 增加 `RunID` 字段；`MessageFromEvent` 从 `Meta.parent_tool_call_id` 读出写入
- [x] 3.2 新迁移 `migrations/014_subagent_run_id.sql`（`messages.run_id CHAR(64) NULL` + 索引评估）；变更说明标注存量库手动应用
- [x] 3.3 mysql 写入/读取与 Redis `msgs:` 缓存行透传 `run_id`（NULL 容忍）；集成测试覆盖往返不丢失
- [x] 3.4 `api/openapi.yaml` SSE 事件 schema 补可选 `meta.parent_tool_call_id`，并同步一份到 blowball-frontend 供重新生成类型

## 4. 重建与展示归属

- [x] 4.1 `message_reconstruct.go` / `MessagesToAgentMessagesIndexed` 按 `(agent, run_id)` 分组重放带身份的行（组内到达序；`run_id` NULL 走现行为）；tool_call/tool_result 配对仍按 `tool_call_id`，单测覆盖交错行 → 分组 → 配对不变
- [x] 4.2 前端（blowball-frontend）：`ui-store.ts` 的 `findSegmentIndex`/`startAgentSegment` 路由键改为 `(agent, runId)`，`use-send-message.ts` 从 `meta.parent_tool_call_id` 取 runId（缺失为空，退化现行为）
- [x] 4.3 前端 `message-list.tsx` `groupMessages` 按复合键切块；验证三个并发同名调用的历史重载呈现为三个独立分段

## 5. 验证与收尾

- [x] 5.1 集成测试：一轮并行 3× `invoke_chongzhi`（fake LLM 交错输出）→ 持久化行带三个 run_id、重建分组后各 run 文本连贯、SSE 事件标注正确
- [x] 5.2 回归：单子 agent 回合与无子 agent 回合行为不变（快照/金数据对比，仅多出可选 meta 键与 NULL 列）
- [x] 5.3 `make lint && make test` 全绿；CLAUDE.md 的 Agent orchestration / SSE streaming / Persistence 段落补充 run 身份说明
