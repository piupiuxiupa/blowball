## Context

当前 blowball 的 `SessionHandler.SendMessage` 会在处理每条用户消息前调用 `MessageService.RecoverMessages` 恢复历史，但恢复结果仅用于判断“是否为首轮”（从而决定是否触发标题生成）。随后调用 `OrchestratorRunner.Handle` 时只传递当前用户消息的字符串内容。

`agent.Orchestrator.Handle` 内部把该字符串包装成 `[]agent.Message{{Role:"user", Content: userMessage}}` 后交给 `Confuse.Run`。`Confuse.Run` 本身支持接收任意长度的聊天历史，但上游从未提供。

持久化层保存的是事件流（`token`、`tool_call`、`agent_start` 等），而不是 LLM 直接消费的 chat message 序列；`tool` 执行结果目前只追加到 agent 内部 `round` 切片中，没有作为事件持久化。这导致即便把历史事件流读出来，也无法直接拼成完整的 OpenAI prompt。

## Goals / Non-Goals

**Goals：**
- 让同一会话的多轮对话具备记忆：模型在每一轮都能看到之前所有的用户、assistant 以及 tool 交互。
- 保持现有事件流持久化架构不变，通过“重建”把事件流转换成 `[]agent.Message`。
- 补全 tool 调用上下文：tool call 和 tool result 作为成对消息进入历史。
- 保持子 Agent 的上下文隔离：Chongzhi/Liang 仍然只能看到 Confuse 传给它们的 task + context。

**Non-Goals：**
- 不引入独立的 prompt cache（避免多副本不一致）。
- 不解决上下文窗口超限问题（不做 summarization / compaction / truncation）。
- 不改变三层存储的写入顺序或失败策略。
- 不改变 SSE 事件对外暴露的类型集合，仅新增 `tool_result` 事件类型。

## Decisions

### 1. `OrchestratorRunner.Handle` 接收 `[]agent.Message` 而不是 `userMessage string`

**方案**：把接口改为 `Handle(ctx, workspaceRoot, skillsDir, userID string, messages []agent.Message, hub *stream.Hub)`。

**理由**：
- 这是最小、最显式的改动，直接让 orchestrator 获得完整历史。
- 测试桩可以精确断言传入的 message 列表。

**替代方案**：在 orchestrator 内部根据 `sessionID` 调 `RecoverMessages`。
- **未采纳**：会把持久化依赖拖入 `agent` 包，破坏包边界，也让单元测试必须构造 message store。

### 2. 在 handler 层做事件流 → chat message 的重建

**方案**：在 `internal/handler` 新增 `MessagesToAgentMessages(prior []model.Message) ([]agent.Message, error)`，把 `RecoverMessages` 返回的 `model.Message` 转成 `agent.Message`。

**理由**：
- handler 已经持有 `msgSvc` 和 `agent` 两种类型，转换放在这里不会引入新的跨包依赖。
- 重建逻辑只依赖持久化后的数据模型，与 agent 内部循环解耦。

**重建规则**：
- `event_type = message` 且 `role = user` → `agent.Message{Role: "user"}`。
- 连续同 agent 的 `event_type = token` 且 `role = assistant` → 合并为一个 `agent.Message{Role: "assistant"}`。
- `event_type = tool_call` + 同 `tool_call_id` 的 `tool_result` → 分别映射为 assistant `ToolCalls` 和 `role = tool` 消息。
- `agent_start` / `agent_end` / `agent_error` 等 marker 事件 → 忽略。
- 只保留顶层 Confuse 的对话事件；子 Agent（Chongzhi/Liang）的 token/tool 事件仅用于 UI/审计，不进入 Confuse 的 prompt。

### 3. 新增 `EventToolResult` 并持久化 tool 输出

**方案**：
- 在 `internal/stream/event.go` 增加 `EventToolResult = "tool_result"`。
- 修改 `ToolCallEvent` 构造函数，额外接收 `tool_call_id` 并把它写入 `Meta["tool_call_id"]`（或修改签名）。
- 新增 `ToolResultEvent(agent, toolCallID, output string) StreamEvent`。
- 在 `Confuse/Chongzhi/Liang` 的 tool dispatch 完成后 emit `ToolResultEvent`。
- `MessageFromEvent` 把 `tool_result` 存为 `role = tool`，content 为 `{"tool_call_id":"...","output":...}`。

**理由**：
- 没有 tool result，历史里的 tool call 就是“悬空的”，会导致模型重复调用或幻觉。
- 以事件形式持久化，复用现有三层存储，无需新增存储结构。

### 4. `tool_call` 持久化内容增加 `tool_call_id`

**方案**：把现有 `{"name":..., "args":...}` 改为 `{"tool_call_id":"...","name":...,"args":...}`。

**理由**：
- 重建时必须把 assistant `ToolCalls` 和 `role = tool` 结果通过 `tool_call_id` 配对。
- `agent.ToolCall.ID` 在 agent 循环内部已经存在，只是之前没有 emit。

**兼容性**：
- 旧数据没有 `tool_call_id`，重建时会被视为“未配对”而跳过该 tool call。这是可接受的退化：旧会话的工具上下文丢失，但不会阻塞新会话。

### 5. 并发 tool call 的合并

**方案**：当一轮里模型并行调用多个 tool 时，事件流会连续出现多个 `tool_call` 事件。重建逻辑应把它们合并为一个 assistant 消息（`ToolCalls` 数组），然后按顺序追加对应的 `tool_result` 消息。

**理由**：
- 这是 OpenAI chat format 的规范表示：一个 assistant 消息可以携带多个 `tool_calls`，随后跟多条 `role = tool` 消息。

## Risks / Trade-offs

- **[Risk] 上下文窗口快速膨胀** → 当前不做 truncation，长会话的 token 用量会线性增长。后续需要独立的 context management 变更。
- **[Risk] 旧会话的 tool call 无法重建** → 旧 `tool_call` 行缺少 `tool_call_id`，会被忽略。文字历史仍可恢复，工具历史需要新会话才能完整。
- **[Risk] SSE 新增 `tool_result` 事件，前端可能不识别** → `tool_result` 事件对前端不是必需信息，可安全忽略；若前端需要展示 tool 结果，可后续消费该事件。
- **[Risk] 重建逻辑出错会导致 prompt 语义错误** → 通过单元测试覆盖各种事件序列（纯文字、单 tool、多 tool、子 agent 事件混入等）来降低风险。
- **[Trade-off] 每次请求都从事件流重建，而不是缓存 prompt** → 计算开销小（单次线性扫描），但避免了独立 cache 的同步问题。

## Migration Plan

- 数据库层面无需迁移：`event_type` 是 `VARCHAR`，新增 `tool_result` 值与旧行兼容。
- 代码层面：`OrchestratorRunner` 接口变更属于内部契约，需要同步更新 `orchestratorAdapter`、生产注入代码以及所有测试桩。
- 部署后：新会话立即获得完整记忆；旧会话的文字记忆可用，工具记忆不完整。

## Open Questions

- `ToolCallEvent` 的签名是直接加 `toolCallID string` 参数，还是把 ID 放进 `Meta` 保持向后兼容？建议直接加参数，因为事件构造是内部调用。
- 子 Agent 的 tool 结果是否也需要进入事件流？当前设计认为不需要，子 Agent 的输出已通过 `tool_result` 汇总回 Confuse；若未来需要审计子 Agent 内部 tool 结果，可再扩展。
