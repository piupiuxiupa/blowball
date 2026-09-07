## Why

OpenAI 兼容网关常在 content chunk 上附带空字符串 `reasoning_content: ""`。当前流式解析层在 unmarshal 成空字符串后仍调用 `onReasoning("")`，导致每个空 delta 都生成一条 `reasoning` 事件：既在 `messages` 表落成空 `content` 的 reasoning 行，又打断了相邻 token 事件的合并，使 content 退化为每个 chunk 一行。三个表象（reasoning 一整条、content 每 chunk 一条、无 reasoning 内容也出一条空记录）同源于此。

## What Changes

- OpenAI 流式解析在 `reasoning_content` unmarshal 结果为空字符串时 SHALL 不调用 `onReasoning` 回调、不累积进 `LLMResponse.ReasoningContent`（对齐 content 分支已有的 `delta.Content != ""` 判空）。
- 空字符串 reasoning delta SHALL 不产生 SSE `reasoning` 事件，也不产生持久化消息行。
- 清理空 reasoning delta 后，相邻 token 事件恢复合并：一个 turn 的纯文本回复在 `messages` 表回到一条 merged `token` 行。
- 存量脏数据（`event_type='reasoning' AND content=''`）不通过代码迁移修复；提供独立手工 SQL 供运维按需执行（不进入 `migrations/`）。

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `message-storage-optimization`: 新增需求——空字符串 reasoning/token delta 不产生持久化行，且空 reasoning delta 不得打断相邻 token 事件的合并语义。

## Impact

- 代码：`internal/agent/openai_client.go`（StreamChat 的 reasoning delta 分支）；`internal/agent` 回归测试。
- 行为：SSE 流不再出现空 `reasoning` 帧；`messages` 表不再出现空 reasoning 行；token 合并恢复。
- API/Schema：无 HTTP 契约变更、无表结构变更、无新迁移文件。
- 运维：存量空行清理 SQL 单独交付（手工执行），不在本 change 代码范围内。
