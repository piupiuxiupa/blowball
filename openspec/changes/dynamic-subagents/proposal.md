## Why

当前多 Agent 编排采用固定角色子 Agent（Chongzhi 写文件、Liang 只读分析），路由规则写死在工具描述里，Confucius 无法按任务形态自主决定派发数量、任务拆分与每个子 Agent 的工具范围。主流 Agent 系统（Claude Code 内置 general-purpose subagent、Codex subagents、OpenAI Responses Multi-Agent）均已收敛到同一模式：子 Agent 是「全新上下文 + 父写的任务 prompt + 可收窄的工具/模型」原语，而非预定义角色。本变更将 Blowball 对齐到该模式，使主 Agent 可动态派发任意多个通用子 Agent。

## What Changes

- 新增单一 `spawn_subagent` 调度工具，取代 `invoke_chongzhi` / `invoke_liang`：参数包含 `task`（必填）、`context`、`name`（归因标签）、`tools`（仅允许从父 Agent 工具集中**收窄**，不得扩权）、`resume_agent_id`（带历史续跑既有子 Agent）。
- 子 Agent 变为通用实现：统一的 tool-calling 循环 + 通用 system prompt，能力完全来自父 Agent 派发时写的任务 prompt 与工具收窄；不再有预置角色画像。
- **BREAKING**（模型面与配置面）：`invoke_chongzhi` / `invoke_liang` 工具消失；`config.yaml` 中 `agents.chongzhi` / `agents.liang` 固定角色段废弃，由 `agents.subagent` 通用默认段 + 可选命名 preset 模板取代。
- 支持带历史续跑（resume）：每次 spawn 返回 `agent_id` 与 `status`（`completed` / `capped` / `error`）；子 Agent 的完整消息历史以快照形式持久化，续跑时以快照为权威历史，已折叠进历史的孙 Agent 不复活。
- 子 Agent 之间完全隔离：无 mailbox、无兄弟通信、无共享黑板，协调只经父 Agent 中转。
- 嵌套派发可配置：`agents.subagent.max_depth`（默认 1 = 现行扁平行为）、`max_concurrent`（全树共享并发预算）、`max_total_per_turn`（全树派生总数上限）；depth=1 时子 Agent 工具列表不含 `spawn_subagent`。
- 事件与用量归因升级：引入稳定的 `agent_instance_id`（跨 resume 不变，UI 线程聚合键）与每次派发独立的 `run_id`（事件隔离键），usage 按动态实例标签 + 父链归因。

## Capabilities

### New Capabilities

- `dynamic-subagents`: 通用子 Agent 派发契约——spawn_subagent 工具参数与结构化结果、通用子 Agent 执行循环、仅收窄的工具授权、全树 depth/concurrency/total 预算、历史快照与 resume 语义、子 Agent 间完全隔离。

### Modified Capabilities

- `agent-orchestration`: flat 拓扑需求改为可配置深度（默认 1 层，行为等价现状）；调度入口由 invoke_chongzhi/invoke_liang 改为 spawn_subagent；子 Agent 上下文隔离需求增加「resume 时以持久化历史快照为起点」分支。
- `subagent-run-identity`: run 身份拆分为稳定 `agent_instance_id`（跨 resume 不变）与每次派发的 `run_id`；事件 Meta 与持久化行同时携带两者，前端聚合键升级为 instance id。
- `turn-cost-tracking`: `usage.by_agent` 的 key 由固定角色名改为动态实例标签（含父链），跨 resume 的同一 instance 累计归因。

## Impact

- `internal/agent`：Confucius 调度开关、Chongzhi/Liang 合并为通用子 Agent、spawn 工具 schema、SubAgentFactory 语义扩展（按 instance 构建或续跑）、全树预算控制器。
- `internal/config`：`agents.subagent` 段（prompt 默认值、max_depth/max_concurrent/max_total_per_turn、可选 presets），固定角色段迁移与校验。
- `internal/store` + `migrations/`：子 Agent 历史快照持久化（新表或新列）、`agent_instance_id` 落库；存量 `run_id` 行为兼容。
- `internal/stream` / `internal/model`：事件 Meta 扩展（instance id + parent 链）、usage breakdown 结构。
- `api/openapi.yaml`：SSE 事件 Meta 新增字段（`agent_instance_id`、父链）为客户端可见契约，需同步。
- 前端（消费方）：分段聚合键从 `(agent, run_id)` 迁移到 instance id；`invoke_*` 相关硬编码需移除。
