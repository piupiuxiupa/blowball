## Context

`internal/agent/openai_client.go` 中的 `OpenAIClient.StreamChat` 是应用与底层 OpenAI 模型交互的唯一入口。上游 `Confuse`、`Chongzhi`、`Liang` 三个 agent 在调用前会追加 system prompt、注入 tools，导致前端事件流和数据库无法反映实际发往模型的原始消息列表。当前该函数没有任何日志，开发者在排查模型行为时只能看到 agent 包装后的结果。

## Goals / Non-Goals

**Goals:**
- 在 `StreamChat` 边界输出足够的结构化 debug 信息，使开发者能看清实际发往模型的请求和模型返回的响应。
- 使用现有的 `logger` 和 `trace` 包，不引入新依赖。
- 保持改动最小，只修改 `internal/agent/openai_client.go`。

**Non-Goals:**
- 不在 HTTP 层做请求/响应 body 的 wire-level 抓取（SSE response body 需要 tee 流，复杂且容易影响性能）。
- 不修改 `LLMClient` 接口、agent 循环、事件流、数据库结构。
- 不把 LLM 日志持久化到数据库或文件系统；仅通过 stdout JSON 日志输出。

## Decisions

1. **日志位置：直接写在 `OpenAIClient.StreamChat` 内**
   - 这里已经拿到 agent 封装后的 `LLMRequest` 和 SDK 转换后的 `params`，能直接反映“底层模型看到的请求”。
   - 循环结束后聚合的 `LLMResponse` 能反映“模型返回的原始结果”。
   - 替代方案（在 `LLMClient` 接口加装饰器）更通用，但用户要求最小改动，直接在当前文件实现成本最低。

2. **日志级别：`logger.L().Debug`**
   - 依赖 `config.logging.level: debug` 开启，默认 `info` 时不输出，避免污染生产日志。

3. **字段设计：请求侧记录结构化摘要，不记录完整内容**
   - 包括 trace_id、agent（从 `req` 中无法直接知道 agent 名，但可通过调用上下文传递；实际实现中不强制要求 agent 字段，优先记录 model/messages/tools/usage）。
   - messages 只记录 role 和截断后的 content preview，防止 system prompt 或工具结果过长导致日志爆炸。
   - tools 只记录数量和名称 preview。

4. **trace_id：从 `ctx` 提取**
   - handler 已通过 `trace.WithContext` 注入 trace_id，使用 `trace.FromContext(ctx)` 取出并写入日志字段。

5. **响应侧记录聚合结果**
   - 流式读取结束后再输出一条响应日志，包括 finish_reason、content 长度/preview、tool_calls 名称/参数 preview、usage。

## Risks / Trade-offs

- **[Risk] 日志量过大** → 仅输出 debug 级别；对长 content 截断到固定长度（如 500 字符）。
- **[Risk] 敏感信息泄露** → debug 环境可接受；生产开启 debug 时需注意用户消息、system prompt、工具结果可能包含隐私/代码。
- **[Risk] 逐 token 的 chunk 不记录，无法看到流式中间过程** → 本设计只记录请求和最终响应；如需逐 chunk 调试，可后续在循环内增加更轻量的 `Debug` 日志（仅记录 delta 存在性/长度）。
- **[Trade-off] 不记录 agent 名** → `LLMRequest` 未携带 agent 字段，如需可在后续迭代中通过上下文或接口扩展传入；本次保持最小改动。
