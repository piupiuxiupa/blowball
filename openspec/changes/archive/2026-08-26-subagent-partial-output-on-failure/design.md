## Context

`Confucius.dispatchSubAgent`（`internal/agent/confucius.go:463`）调用子 agent `Run` 后，仅在 `err == nil` 时采用返回的 content；所有失败路径只返回 `err.Error()`。而子 agent 侧（`chongzhi.go` / `liang.go`）的错误返回值形同虚设：

- `finalContent` 的赋值点全部位于 `break` 路径（空内容中断、自然停止）与 wrap-up 成功路径 —— llm_error / ctx 取消发生时循环没有 break 过，`finalContent` **恒为空串**；
- 失败轮真正已产出的内容是 `assistantText`（round 作用域、onToken 回调累计的已流出 token），错误返回时被丢弃。

两层同时失效导致：跑了多轮工具调用、产生过文件副作用的子 agent 一旦失败，其全部产出对 Confucius 不可见（仅 SSE 流里可见）。`length_exhausted` 是唯一例外 —— 它已经返回 `result.Content`（跨 continuation 尝试的累积部分输出），但该值同样在 `err != nil` 时被 dispatchSubAgent 丢弃。

约束（保持不变）：
- 子 agent 结果绕过 tool-result-envelope（`{"status":...}` 包装），content 是裸文本；
- `isError: true` 语义、重试判定（`shouldRetry`、`LastRunExecutedTool` 门控、retryBudget）、usage 折叠到 `total`/`byAgent` 均不动；
- SSE 事件结构与持久化 schema 不变。

## Goals / Non-Goals

**Goals:**

- 子 agent 失败被放弃后，Confucius 收到「错误文本 + 已积累部分输出」的合并 tool result；
- 部分输出为空白时，tool result content 与现状**字节级一致**（零行为回归）；
- SSE `tool_result` 事件与持久化行自动携带同一合并文本（用户与未来 turn 均可见）。

**Non-Goals:**

- 不改重试判定与重试次数 —— 拼接只发生在放弃点，不影响「是否重试」的决策；
- 不抢救 `round_cap_exhausted` 中 wrap-up 轮的部分输出（困在 `runWrapUpRound` 内部，需改签名扩大范围，且该路径 `finalContent` 恒空、价值低）；
- 不引入子 agent 中间轮 tool 调用过程（本地 `round` 消息）的回传 —— 只回传 assistant 文本；
- 不改 Confucius 自身错误路径的部分输出透出（Confucius 不是子 agent，其错误走 orchestrator/done 事件，另一条链路）；
- 不改 envelope 约定、不加配置项、不动 schema。

## Decisions

### D1: 两层修复，缺一不可

- **Layer 1（子 agent Run 错误返回）**：`chongzhi.go` / `liang.go` 的 llm_error 与 ctx 取消路径改为返回 `assistantText`（失败轮已流出的部分），替代恒空的 `finalContent`。
- **Layer 2（放弃点拼接）**：`dispatchSubAgent` 的 4 个失败返回点用 `joinFailure(err, content)` 拼接。

*理由*：只改 Layer 2 的话，llm_error 路径拿到的 content 恒为空，拼不出任何东西；只改 Layer 1 的话，返回值在 `err != nil` 分支被丢弃。备选「让 dispatchSubAgent 从 SSE 事件流回收 token」被否决 —— 事件已经流走，回收需要额外缓冲与并发协调，复杂度不成比例。

### D2: 按 error path 区分可抢救内容

| 错误路径 | 返回的部分输出 | 改动 |
|---|---|---|
| llm_error（stream chat 失败） | `assistantText`（失败轮已流出 token） | Layer 1 改 |
| ctx 取消 | `assistantText` | Layer 1 改（一致性；Confucius 同样在死，无害） |
| `length_exhausted` | `result.Content`（跨 continuation 尝试累积） | 已实现，仅 Layer 2 生效 |
| `round_cap_exhausted` | 无（`finalContent` 恒空） | 不动 |

### D3: `joinFailure` 格式与空白兼容

```go
// content 空白（"" 或全空白）→ err.Error()，字节级不变
// 否则 → err.Error() + "\n\n--- partial output before failure ---\n\n" + content
```

- 分隔符是稳定的英文标记行，模型可读、前端按文本渲染无需特判；
- 空白判断用 `strings.TrimSpace`，避免只含空白的垃圾片段触发拼接；
- 备选「结构化 JSON envelope 包错误与部分输出」被否决 —— 违反子 agent 结果绕过 envelope 的既有约定，且对模型的直接消费不如裸文本。

### D4: 只在放弃点拼接，重试间不拼

`retryErrorEvent`（前端重试提示）只带 err，不带部分输出 —— 重试意味着还要再跑，拼了也会被下一次 Run 覆盖。重试循环中 `content, usage, _, err = sub.Run(...)` 每轮覆盖 content，放弃点天然持有**最后一次尝试**的部分输出，无需额外状态。语义与「Retries reuse the SAME instance (one logical call)」一致：前几次尝试的部分输出不保留。

### D5: SSE / 持久化 / 跨 turn 可见性走既有通道

不为合并文本造新事件：Confucius 名下的 `tool_result` SSE 事件（`confucius.go:422`）与持久化行直接携带合并后的 content。未来 turn 历史重建时，子 agent 名下事件行照旧被跳过（`message_reconstruct.go:65`），内容以 Confucius 的 tool_call/tool_result 对进入 —— 跨 turn 与 turn 内看到的失败结果一致。

## Risks / Trade-offs

- [Confucius 上下文膨胀：部分输出可能很长（`length_exhausted` 时可达 quota + continuation 扩张量）] → 该文本本就流经 SSE；turn 内受 mid-turn compaction 80% 阈值约束；失败是异常路径，非稳态开销。
- [模型把半成品当完整答案] → 错误文本在前 + 明确的分隔标记行，失败事实对模型可见。
- [合并文本与现存测试断言冲突] → grep 确认 `internal/agent/*_test.go` 无 `isError` 文本断言；新增测试锁定「空白时字节级不变」防回归。
- [前端展示合并文本的观感] → 前端按普通 tool_result 文本渲染，无 schema 依赖；用户反而获得了原本只能翻 token 流才看得到的信息。

## Migration Plan

纯代码行为变更：无 schema、无 API 契约、无配置项变更，正常部署即生效。回滚 = revert 该 commit，无数据迁移考虑（持久化的 tool_result 行内容随新行为变化，旧数据保持旧形态，两种形态对历史重建都是普通文本）。
