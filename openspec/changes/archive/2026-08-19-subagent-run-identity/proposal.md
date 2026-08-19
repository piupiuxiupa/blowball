## Why

当 Confucius 在同一回合并行派发多个子 agent 调用（如三条并行的 `invoke_chongzhi` 数据收集流）时，同名调用共享**一个**子 agent 实例和**一个**流身份（事件只有 `Agent` 显示名）。三路的 token/reasoning 事件按到达顺序交错：`MergeEvents` 把相邻同 agent 碎片粘住、两个 run 的句子被拼成半截话落库，前端把三路文本拼进同一个分段——用户看到的是中英文互相穿插的乱码式输出（实证：session `01a01457-f5dc-73ae-8079-4aa151c8d2ec`，一轮 3× `invoke_chongzhi`，messages idx 20–30）。更严重的是伴生正确性 bug：共享实例的 `executedToolThisRun`/`hitCapThisRun` 在并发 Run 间互相覆盖/串读，导致重试幂等保护失效与 `round_capped` 误报。

## What Changes

- **每次 invoke 新建子 agent 实例**（方案 B）：`dispatchSubAgent` 为每次调用构建独立的 Chongzhi/Liang 实例（不再复用 factory 每 turn 单例），并发同名 Run 不再共享可变状态；重试幂等（副作用后不重试）与 round-cap 传播变为每次调用的独立保证。
- **事件流加调用身份**（方案 C）：每次子 agent 调用以其 invoke tool_call 的 `tc.ID` 作为 run 身份，通过包装 hub 给该次 Run 的全部事件 `Meta` 注入 `parent_tool_call_id`；子 agent 代码零改动。SSE 事件与 `api/openapi.yaml` 契约透出该字段。
- **持久化**：`messages` 表新增 run 身份列（新迁移；存量库按惯例手动应用），交错落库的行可按 run 归属重组。
- **重建与展示**：历史重建（`message_reconstruct`）按 run 归属交错行；前端（兄弟仓库 blowball-frontend）`streamingSegments` 与 `groupMessages` 按 `(agent, run_id)` 分段路由，替代当前"按名字找活动段"的复用逻辑。
- **明确不做**：同名调用串行化（方案 A）——并行度保留，交错问题靠身份归因根治。

## Capabilities

### New Capabilities
- `subagent-run-identity`: 每次子 agent 调用携带端到端唯一的 run 身份（流事件 → 持久化 → 展示分段），并发调用（含同名）全程可区分、可归属、互不污染。

### Modified Capabilities
- `agent-orchestration`: 并行子 agent 分发必须使用按调用隔离的 agent 实例——"副作用后不重试"的幂等判定与 `round_capped` 传播从"每实例"改为"每次调用"的保证（现有 Parallel agent execution / Transient error retry 需求在并发同名场景下的语义收紧）。

## Impact

- **后端**：`internal/agent/confucius.go`（dispatchOne/dispatchSubAgent、run 标注 hub）、`internal/agent/orchestrator.go`（子 agent 构造时机）、`internal/stream/`（包装 hub 或等价机制）、`internal/handler/event_mapper.go` + `internal/model` + `internal/store/mysql`（新列读写）、新迁移 `migrations/0xx`、`api/openapi.yaml`（SSE 事件 schema 增加 `meta.parent_tool_call_id`）。
- **前端**（兄弟仓库 blowball-frontend，随本变更同步改）：`src/stores/ui-store.ts`（分段键改为 `(agent, run_id)`）、`src/hooks/use-send-message.ts`（事件路由）、`src/components/chat/message-list.tsx`（`groupMessages`）；需重新生成 `src/lib/openapi.d.ts`。
- **运维**：存量数据库需手动应用新迁移；单调用回合无行为变化（run 身份仅出现在子 agent 事件上）；Redis `msgs:` 缓存行结构不变（新列仅落 MySQL 行与 API 载荷）。
