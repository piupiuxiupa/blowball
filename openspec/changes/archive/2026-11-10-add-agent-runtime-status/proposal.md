## Why

当前 `max_rounds` 只在耗尽后触发一次禁用工具的收尾请求，模型在执行过程中无法得知还剩多少工具轮次，因而不能提前安排收尾。同时，发送用户消息时注入的时间状态被直接拼接到用户原文并随之持久化；模型也缺少与本轮请求预算一致的上下文余量提示。需要把这些运行时状态持续提供给模型，但不能让它们污染真实会话、标题和用户消息序号。

简单的“每次替换请求尾部状态”会使上一次完整提示词无法成为下一次提示词的前缀。供应商提示词缓存本身就是临时的，因此更合适的方式是：活跃会话在 Redis 中保留不可变、append-only 的状态消息，MySQL 不保存这些消息；Redis miss 或长时间不活跃后允许从干净 MySQL 历史开始新的缓存纪元。

此外，压缩的 Tool Activity 台账目前只按宿主工具名聚合，所有 `mcp_call` 被折叠成一行总数，实际 MCP server 与 remote tool 的调用分布在压缩后丢失。需要在现有确定性台账中保留每个服务下每个工具的统计，而不是依赖摘要模型重新计数。

## What Changes

- 在 Confucius 与通用 SubAgent 的工具循环中增加基于现有 `max_rounds` 的轮次倒计时：一轮中的全部并行工具结果到齐后，追加独立的 `tool_budget` synthetic user 消息；不新增“单个工具调用次数”硬上限，现有 round-cap wrap-up 行为保持不变。
- root 的时间和上下文状态、工具轮次状态采用 **append-only**：已发送的状态消息在活跃 turn 内及 Redis 命中后的后续 turn 中逐 byte 保持不变，新状态只追加到末尾，旧状态不修改、不删除。
- 在 Redis 中增加独立的 session-scoped 状态缓存，以 canonical 用户消息或 tool_call 为锚点恢复状态位置。现有 `msgs:{sid}` 继续只缓存 MySQL 消息行，状态不冒充真实用户记录，也不参与标题用户 ordinal。
- MySQL、子 Agent run transcripts、SSE 和前端历史不保存或展示这些 synthetic 状态；既有 `llm_raw_log` 仍按实际请求原样捕获。
- Redis 状态缓存命中时，按原内容、原顺序、原锚点重放；未命中、锚点失效或缓存不完整时，从 MySQL 干净历史开始新的缓存纪元并生成新状态。该降级只影响供应商缓存命中，不影响对话正确性。
- 将时间状态从用户原文拆出，在独立 `<current_status>` 中加入基于完整候选请求的上下文使用状态。真实用户消息、send-time 持久化、标题生成及用户消息序号只使用用户原文；已有历史行不做回填或迁移。
- compaction 可以丢弃其改写范围内的状态并清空旧状态缓存，因为压缩本身已经重写供应商前缀；压缩后在新的 canonical projection 末尾追加最新状态。
- 扩展确定性 Tool Activity 台账：保留 `mcp_call` 父级总次数，同时从原始 arguments 解析 `server`/`tool`，按服务分组记录每个 remote tool 的调用、成功和失败次数，并在再次压缩时按同一维度合并。
- 旧压缩记录中只有 `mcp_call` 总数的部分保留为未归因计数，不伪造历史明细；普通工具、其他 `mcp_*` 管理工具及 operator MCP 代理工具的现有统计口径不变。
- 不修改压缩摘要提示词，不新增 HTTP API、MySQL migration 或配置项。

## Capabilities

### New Capabilities

- `agent-runtime-status`: 定义 Redis-only、append-only 的模型运行时状态、缓存纪元与锚点重放、工具轮次倒计时、独立时间/上下文状态、用户原文隔离及持久化排除契约。

### Modified Capabilities

- `agent-orchestration`: 在现有 `max_rounds` 上限和 tool-disabled wrap-up 之前增加 Confucius/SubAgent 的逐轮剩余轮次提示，不改变上限的计数语义。
- `message-final-persistence`: 明确运行时状态不属于 MySQL durable 消息；Redis 命中时的状态重放与 miss 时的新缓存纪元是前缀稳定性契约的显式组成部分。
- `context-request-budgeting`: 完整请求估算必须包含当前所有 cache-only 状态及新状态自身，压缩候选只保留压缩后仍会存在的状态开销。
- `context-compaction`: Tool Activity 从仅按宿主工具名统计扩展为同时保留 `mcp_call` 的 server/tool 嵌套明细，并定义再次压缩合并、旧记录未归因余量和总数不重复计算的规则。

## Impact

- 主要影响 `internal/turn/executor.go`、`internal/prompt/render.go`、`internal/agent/confucius.go`、`internal/agent/subagent.go`、`internal/service/tool_activity.go`、Redis 会话缓存接口及删除/压缩清理路径。
- 需要新增独立的 Redis runtime-status 缓存模型与恢复合并逻辑，并以 canonical client message id 或 tool_call id 作为锚点；不改变现有 `msgs:{sid}` 的 MySQL 行缓存含义。
- 需要同步更新模型请求、重试、continuation、SubAgent snapshot、compaction、live overlay、压缩台账和请求预算测试，以及 `CLAUDE.md`、`AGENTS.md`。
- 不改变 `max_rounds` 配置含义，不引入 `max_tool_calls`，不改变 SSE/API schema、MySQL 表结构或模型目录。
- `context_compactions.tool_activity_json` 仅增加可选 JSON 明细字段，保留现有父级字段以兼容旧记录和回滚，无需 schema migration。
- 新发送的用户消息不再把 `<current_status>` 拼入持久化正文；旧消息保持原样读取。
