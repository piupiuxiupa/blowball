## Why

在同一会话中连续提问时，Agent 无法引用之前的对话内容。问题的根源不是消息没有被持久化，而是 `Orchestrator` 每次只把当前用户消息传给 `Confuse.Run`，历史消息在 handler 层被读取后没有继续向下传递，导致模型每轮只看到单轮上下文。

## What Changes

- **BREAKING** 修改 `OrchestratorRunner.Handle` 与 `agent.Orchestrator.Handle` 的接口，由接收单条 `userMessage string` 改为接收完整的聊天历史 `messages []agent.Message`。
- 在 `SessionHandler.SendMessage` 中把 `RecoverMessages` 恢复的历史消息加上当前用户消息一并传给 orchestrator。
- 新增从持久化事件流（`[]model.Message`）重建 `[]agent.Message` 的能力：合并相邻 token 为 assistant 消息、识别 user 消息、忽略 marker 事件。
- **BREAKING** 扩展消息事件模型，新增 `tool_result` 事件类型；Agent 在拿到 tool 执行结果后 emit 并持久化该事件，使 tool call / tool result 能成对进入历史。
- 更新相关 handler/agent 单元测试和集成测试，覆盖多轮对话记忆场景。

## Capabilities

### New Capabilities

- `agent-conversation-memory`: 定义如何从 `MessageService.RecoverMessages` 返回的事件流重建 OpenAI chat-format 的 `[]agent.Message`，包括 token 合并、tool call / tool result 配对、marker 事件过滤。

### Modified Capabilities

- `agent-orchestration`: 新增要求——编排层 SHALL 接收并向下传递完整会话历史，Confuse 的主循环基于历史 + 当前用户消息启动；子 Agent 仍保持独立上下文。
- `session-management`: 扩展消息数据模型，新增 `event_type = tool_result`；`RecoverMessages` 返回的序列 SHALL 包含 tool result 行；`Send message` 流程 SHALL 使用历史消息构建 prompt。

## Impact

- `internal/handler/ports.go` 接口变更，所有实现/桩需要同步更新。
- `internal/handler/session.go`、`internal/agent/orchestrator.go` 核心调用路径变更。
- `internal/handler/event_mapper.go` 增加事件到 prompt message 的映射逻辑。
- `internal/stream/event.go` 增加 `EventToolResult`。
- `internal/agent/confuse.go`、`internal/agent/chongzhi.go`、`internal/agent/liang.go` 在 tool 调用完成后 emit tool result 事件。
- 测试：`internal/handler/session_test.go` 桩、`test/integration/message_flow_test.go` 需要新增多轮场景。
