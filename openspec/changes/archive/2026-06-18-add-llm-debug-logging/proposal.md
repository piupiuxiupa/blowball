## Why

当前 LLM 调用链路中，agent 在 `Run()` 里会追加 system prompt、注入 tools，再把 `LLMRequest` 交给 `OpenAIClient.StreamChat`。前端事件流和数据库只保留 agent 包装后的最终内容，开发者在 debug 时无法直接看到实际发到底层模型的消息列表以及模型返回的原始结构，排查问题困难。

## What Changes

- 在 `internal/agent/openai_client.go` 的 `StreamChat` 中增加结构化 debug 日志。
- 请求侧记录：trace_id、agent/model、消息数量及每条消息的 role + 截断 preview、tools 数量、max_tokens。
- 响应侧记录：trace_id、finish_reason、content 长度/preview、tool_calls 名称及参数 preview、usage。
- 日志使用 `logger.L().Debug`，仅在 `logging.level: debug` 时输出，避免污染 info 日志。
- 对长 content 做截断，防止 debug 日志过大；不记录 API key。

## Capabilities

### New Capabilities

- `llm-debug-logging`: 当应用日志级别为 debug 时，在 `OpenAIClient.StreamChat` 边界输出请求与响应的原始结构化信息，帮助开发者看清 agent 封装后的真实 LLM 输入输出。

### Modified Capabilities

- 无（本次为内部可观测性增强，不改动现有行为或接口）。

## Impact

- 仅影响 `internal/agent/openai_client.go`。
- 不修改 `LLMClient` 接口、agent 循环、事件流或数据库结构。
- 依赖已有的 `internal/pkg/logger` 和 `internal/pkg/trace`。
