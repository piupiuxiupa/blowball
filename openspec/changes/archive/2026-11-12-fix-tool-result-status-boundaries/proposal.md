## Why

生产提交 `9b18297e82732e095f479978c184448cd4b3ee58` 的历史重建可能把上一 turn 的工具调用与下一 turn 的 send-time 记忆召回合并为同一个 assistant 工具单元；runtime status 回放随后按原工具 ID 插入 `user/tool_budget`，打断尚未完整的 tool results，导致上游返回 MaaS 400。2026-11-11 的日志 A/B 验证中，仅交换 `messages[28]` 与 `messages[29]`、保持其余请求字段一致，响应即由 400 变为 200，需要在重建与回放边界修复，而非调整并行工具等待逻辑。

## What Changes

- 在 canonical 历史重建中保留 assistant round 与 turn 边界，禁止把相邻、不同 turn 的工具占位行合并；下一 turn 的 synthetic `recall_memories` 保持独立的 assistant/tool 对。
- 保留同一 assistant round 内真正并行工具的完整分组、调用顺序、结果顺序及 narrated content/reasoning，不按单个工具结果完成时机拆组。
- 加强 runtime status 的 canonical 锚点校验：`after_tool_call_id` 必须位于对应完整工具结果组的末尾，before-user 锚点必须实际对应真实用户消息，回放结果不得被状态拆组或改变状态序号顺序。
- 任一状态锚点或回放顺序无效时拒绝整份状态日志，沿用 WARN、best-effort 清理和新缓存纪元降级；不移动旧状态猜测位置，不修改 MySQL 或 `msgs:{session_id}`。
- 增加“工具结束的上一 turn + 下一 turn 召回 + status replay”组合回归，以及真实并行、前缀稳定和非法缓存降级测试。

## Capabilities

### New Capabilities

无。

### Modified Capabilities

- `message-final-persistence`: 明确 canonical 工具单元不能跨 assistant round/turn 合并，并覆盖无后续 assistant 文本的相邻工具占位行。
- `memory-recall-replay`: 保证 send-time synthetic recall pair 在历史恢复中仍是独立单元，不能吸收上一 turn 的工具调用。
- `agent-runtime-status`: 将锚点存在性校验补齐为完整工具组边界及回放顺序校验；非法状态日志整体失效而不打断协议消息。

## Impact

- 实施基线固定为生产提交 `9b18297e82732e095f479978c184448cd4b3ee58`；提案与后续修复在该提交创建的独立工作树中进行，不合入当前主工作区的调度器服务抽取或其他无关改动。
- 主要修改点为 `internal/turn/message_reconstruct.go`、`internal/turn/runtime_status.go` 及相关 turn/handler 回归测试；agent 侧只补充必要的并行状态时序测试，保持现有 `g.Wait()` 与 `max_rounds` 计数逻辑不变。
- 不变更 HTTP/OpenAPI、SQL schema、Redis 状态记录格式、配置项、依赖或工具定义。现有 canonical 行与旧状态日志通过重建和校验处理，不做数据迁移。
- 原始请求日志、配置凭据及临时诊断脚本不进入提交；测试使用最小、脱敏的结构性 fixture，不自动调用生产上游。
