## Why

子 agent 失败时，`dispatchSubAgent` 只把错误字符串作为 tool result 返回给 Confucius，子 agent 已积累的部分输出被静默丢弃。最疼的场景：Chongzhi 跑了十几轮工具调用、已经写过文件，最后一轮 LLM 调用失败 —— Confucius 只收到一句 `chongzhi: stream chat: ...`，整个工作对主 agent 不可见，只能重头再来或放弃。同时子 agent `Run` 在 llm_error 路径返回的 `finalContent` 恒为空串（赋值点全在 break 路径），失败轮已流出的 `assistantText`（round 作用域）也被丢弃 —— 部分产出根本没机会到达拼接层。

## What Changes

- **子 agent `Run` 的错误返回携带部分输出**（`internal/agent/chongzhi.go`、`liang.go`）：llm_error 与 ctx 取消路径返回失败轮已流出的 `assistantText`（现状恒返回空 `finalContent`）。
- **`dispatchSubAgent` 放弃返回点拼接错误与部分输出**（`internal/agent/confucius.go`，4 处：`!shouldRetry` 早退、backoff 中 ctx 取消、重试耗尽/非瞬断 break）：`joinFailure(err, content)` —— 部分输出为空白时字节级保持 `err.Error()` 不变（旧行为兼容），否则 `err.Error() + "\n\n--- partial output before failure ---\n\n" + content`。
- **自动收益，零额外代码**：SSE `tool_result` 事件、持久化消息行、未来 turn 的历史重建都携带同一段合并文本（子 agent 名下的事件行仍被历史重建跳过，内容以 Confucius 名下的 tool_result 进入）。
- **不变**：`isError: true` 语义；子 agent 结果绕过 tool-result-envelope 的约定；重试判定（`shouldRetry`、`LastRunExecutedTool` 门控、budget）；usage 折叠；`length_exhausted`（已返回全量部分输出）；`round_cap_exhausted`（无可抢救内容，wrap-up 轮的部分输出困在 `runWrapUpRound` 内，不扩大范围）。

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `agent-orchestration`: "Agent as tool via function calling" 需求下失败结果的行为变化 —— 失败的子 agent 调用返回「错误 + 已积累部分输出」而非仅错误文本；部分输出为空时行为不变。

## Impact

- **代码**：`internal/agent/chongzhi.go`、`internal/agent/liang.go`（Run 错误返回值）、`internal/agent/confucius.go`（dispatchSubAgent 放弃返回 + `joinFailure` 辅助函数）。
- **测试**：`internal/agent/` 新增覆盖（llm_error 返回 assistantText、三个放弃路径的拼接与空白兼容）；现存测试无 `isError` 文本断言，影响轻。
- **下游可见性**：前端无需改动（tool_result 内容按文本渲染，用户反而能直接看到子 agent 死前的产出）；持久化 tool_result 行内容变化对未来 turn 的 Confucius 上下文可见（预期行为）。
- **不涉及**：数据库 schema、API 契约、配置项、SSE 事件结构。
