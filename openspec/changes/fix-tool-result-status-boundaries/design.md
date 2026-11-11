## Context

本变更仅以生产提交 `9b18297e82732e095f479978c184448cd4b3ee58` 为实施基线；当前主工作区的后续提交和调度器服务改动不属于本变更。提案阶段只写 OpenSpec 文档，业务代码尚未修复。

生产路径分为三层：

1. `internal/agent/confucius.go` 与 `subagent.go` 经 `g.Wait()` 等待全部工具完成，再按调用顺序追加全部结果，最后追加一次预算消息；该顺序不是本次故障源。
2. `internal/turn/message_reconstruct.go` 通过 messages 中的 placeholder 与 tool_calls payload 重建 canonical transcript。placeholder 分支只在待合并文本的 TraceID 改变时关闭单元，未覆盖已经积累工具调用、但无文本的跨 turn 边界。
3. `internal/turn/runtime_status.go` 以 `AfterToolCallID` 匹配某条 tool 消息后立即插入状态，只校验锚点命中，未确认该消息仍是完整工具结果组的末尾。

已观察到的最小故障形状为：

```text
原 live 单元：assistant[A] → tool[A] → budget[A]
新 turn 注入：assistant[recall-B] → tool[recall-B] → current_status → user

错误重建：assistant[A, recall-B] → tool[A] → tool[recall-B]
错误回放：assistant[A, recall-B] → tool[A] → user budget[A] → tool[recall-B]
```

2026-11-11 的上游 A/B 对照对两个请求使用相同的 32 输出 token 限制，仅交换原消息列表的第 28、29 项，原顺序返回相同 MaaS 400，结果连续后返回 200。该实验确认消息位置是触发因素；实验中的重排不作为生产修复方案。原日志与凭据不随提案或测试提交。

## Goals / Non-Goals

**Goals:**

- 从生产基线修复相邻不同 turn 工具占位行的错误合组，保留同 round 真正并行工具的完整单元和 provider 原始顺序。
- 保证 send-time synthetic recall pair 与上一 turn 的工具单元分离，保留 live 与 recovered canonical 前缀及正确 row attribution。
- 只有语义位置、唯一命中和序号顺序都有效的状态日志才能整体回放；任何状态都不能插入尚未完整的工具结果组。
- 非法 cache-only 日志按现有 WARN、best-effort 清理和新缓存纪元降级，真实历史和 turn 执行不受缓存故障影响。

**Non-Goals:**

- 不重写并行调度、SSE、length continuation、工具定义或 `max_rounds` 计数。
- 不做发送前全局排序、重新渲染历史、清洗用户正文或迁移 MySQL/Redis 数据。
- 不增加配置开关、缓存字段、SQL migration、依赖或客户端 API。
- 不基于 `recall-` 字符串前缀特判工具，不在每个 placeholder 返回结果后强制 flush，也不改变遗留 run_id 的分组约定。

## Decisions

### 1. 在 canonical 重建入口维护单元的 turn 边界

选择在重建状态中保留当前 assistant/tool 单元已知的来源 TraceID，包括其关联的 assistant 终稿与工具占位行。新的 placeholder 属于另一已知 TraceID 时，先关闭上一单元，再构建新的单元；即使上一单元没有 token/reasoning，也不能漏掉这一边界。

已有 assistant final-row、真实 user-row 和遗留 tool-result 后新 call 的 round 边界仍然生效。同一 TraceID、同一 assistant round 内的多个 placeholder 仍积累为一个 assistant tool_calls 数组及其全部结果，不能因为每个 placeholder 已取到 canonical result、`afterResult` 为 true 就拆组。

来源 TraceID 缺失的遗留行沿用已有可确定的 round 边界，不凭工具名、call ID 前缀或单个完成时机发明新边界。重建状态关闭后清理该单元身份，避免上一 turn 的身份污染后续单元。

配对仍使用现有 canonical tool-call 数据，保留参数、结果、content/reasoning 原字节和调用顺序。拆分后的每个 assistant/tool 单元只覆盖自身的 row attribution；压缩不得把该单元的 assistant 与结果拆开。

**Alternatives considered:**

- 只把预算延迟到重建后整组末尾：可消除此次 400，但会掩盖跨 turn 合组，并移动不可变状态的原锚点、破坏 live 前缀；不采用。
- 每个已取回结果的 placeholder 后 flush：会把真正同 round 并行工具拆为多个 assistant 消息；不采用。
- 专门识别 `recall-` ID：只能遮盖本次样例，不能处理其他不同 turn 的相邻工具行；不采用。

### 2. 状态锚点必须代表合法协议位置，而不只是存在

在 `replayRuntimeStatus` 内，利用已重建的 assistant tool_calls 与其连续 tool results 确定完整工具组和组尾，校验：

- `tool_budget` 的 `after_tool_call_id` 唯一指向某个完整工具组的最后结果；存在于组中间、孤立结果或未完整组的 ID 不是合法锚点。
- `current_status` 的 `before_client_msg_id` 唯一指向 row attribution 对应的真实 user 消息；仅在其他消息来源行找到同名 ID 不视为命中。
- 状态 kind 与锚点类型一致，保留现有非空字段、唯一 status ID、单调 Seq 及全部条目命中校验。
- 合并后的状态出现顺序仍对应原日志顺序，使用真实 Seq；合法序号有间隔时继续从最大 Seq 续接，不用条数推断。

只返回完整、校验成功的 candidate。任一失败返回 replay 无效信号，让现有 `replaySessionRuntimeStatus` 负责 WARN 和 best-effort 清理；不把失败变成 turn error，不返回部分状态，不重锚定或移动旧状态。状态日志为空时继续使用既有 canonical 历史，不对它做排序或修复。

**Alternatives considered:**

- 仅判断下一条消息不是 tool：无法证明该 assistant 声明的全部工具已返回，也不能防止错配锚点；不采用。
- 在 LLM 客户端发送前统一排序/修复：作用面大、改变已经发送过的历史并掩盖重建缺陷；不采用。
- 只修重建、不校验 replay：当前样例可恢复，但错误/旧缓存仍可产生中间锚点；不采用。

### 3. 用生产基线的组合 fixture 证明修复，不依赖线上上游

先补回归，再修改实现。最小 fixture 包含上一 turn 的 tool-only 终稿及已完成 A、下一 turn 的 synthetic recall placeholder/B 与真实用户行，以及锚定 A 的预算状态。重建并回放后必须得到两个独立工具单元，A 的 budget 仍在原单元之后；不能得到一个被 user 状态拆开的 `[A, B]` 单元。

另外覆盖同 round 多工具及逆序完成、同 turn 连续 tool-only round、narrated content/reasoning、遗留与新行混合、空 completed result、压缩 row attribution，以及组中间锚点、错误 before-user 锚点、序号顺序无效和 cache cleanup 失败的降级。协议检查和前缀比对只作为本地测试断言，不新增生产中的全局重排层。

## Risks / Trade-offs

- [误把同 round 并行 placeholder 拆组] → 仅使用已知 turn/assistant round 边界；专门保留多个 placeholder 的同组及调用顺序测试。
- [跨 turn 状态身份在 flush 后残留，或 narration 被附到新召回] → 单元关闭时同步重置来源身份，测试无文本工具尾部及带 content/reasoning 的既有前缀。
- [拆组后 row attribution 或压缩边界偏移] → 验证每个独立单元覆盖自身来源行，继续通过完整工具单元保留尾部的现有测试。
- [旧/损坏状态日志被拒绝后损失一次供应商前缀缓存命中] → 这是新缓存纪元的明确降级；保留 canonical 原字节，不牺牲请求合法性猜测旧状态位置。
- [遗留 TraceID 缺失，无法可靠推断新的 turn 边界] → 保持既有遗留重建规则，不迁移或虚构 turn；若其缓存锚点不合法，只作废状态日志。
- [日志还包含其他历史问题] → 仅将已确认的结构性故障做成脱敏 fixture，不把整份生产日志固化为期望输出。

## Migration Plan

1. 在 `9b18297e82732e095f479978c184448cd4b3ee58` 基线补齐回归并确认失败，再实现两层修复；提交只包含本变更源码、测试及 OpenSpec 文档。
2. 通过严格 OpenSpec 校验、相关包测试、`make test` 和 `make lint` 后部署补丁，不附带数据库、配置或 Redis schema 迁移。
3. 旧 canonical 行在恢复时自动使用新边界逻辑；能够正确重建且锚点合法的旧状态日志继续原样回放，不全量清空会话缓存。
4. 非法状态日志只对对应 session best-effort 清理并开启新 epoch；记录 WARN，不修改真实历史。
5. 回滚仅回滚应用补丁，无数据迁移需要撤销；回滚会重新暴露原顺序缺陷和旧缓存风险，不能把删真实历史作为回滚步骤。

## Open Questions

- 无阻塞提案的问题。生产样例中上一 turn 为何没有后续 assistant 文本，仅凭请求体无法区分取消、错误、崩溃或其他结束路径；修复不依赖该原因，测试覆盖工具尾部恢复。
- 具体状态字段和辅助函数命名在实施时选择最小、符合生产基线现有风格的方案；不引入通用重建框架。
