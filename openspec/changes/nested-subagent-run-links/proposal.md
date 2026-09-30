## Why

历史加载时，前端只能从 messages 表的 spawn 占位行拿到第一层子 Agent 的 `agent_instance_id`；当子 Agent 又派生孙 Agent 时，孙实例 id 在任何读取接口中都不出现——`subagent_instances.parent_instance_id` 已完整记录父子关系，但 run 列表/详情 DTO 丢弃了它，transcript 中的 `spawn_subagent` tool call 也无法关联到子实例。结果是刷新后嵌套层内容永远无法懒加载。

## What Changes

- run 列表与详情响应新增 `parent_instance_id`（顶层派生为空/省略），使前端能确定每个实例在树中的位置。
- run 详情的 transcript DTO 中，`spawn_subagent` 类型的 assistant tool call 新增子实例链接字段（子 `agent_instance_id` + 名称），由已持久化的 `(session_id, parent_instance_id, run_id)` 关联查出，前端据此递归懒加载下一层。
- store 读取面新增按父实例解析子 run 身份的查询；不改动写入路径、表结构与迁移。
- 同步 `api/openapi.yaml` 中两个 sub-agent run 端点的响应 schema。
- 不新增路由；不动 SSE 事件与 done meta（live 侧已带全量调用树）。

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `subagent-run-transcripts`: run 列表/详情 API 的响应契约扩展 `parent_instance_id`；safe transcript DTO 为嵌套 spawn tool call 暴露子实例链接，且该链接只能来自同 session 内真实的父子 run 关联。

## Impact

- `internal/store/mysql/subagent_run.go`：新增子 run 身份查询（复用 `subagent_runs` 的 `parent_instance_id`/`run_id` 列，无 schema 变更）。
- `internal/service/subagent.go`：`SubAgentRunSummary`、`SubAgentRunDetail`、`SubAgentToolCall` DTO 扩展；`GetRun` 组装子实例链接。
- `internal/handler/subagent.go`：响应结构随之扩展（handler 本身逻辑基本不变）。
- `api/openapi.yaml`：`SubAgentRunListResponse` / `SubAgentRunDetail` 相关 schema 更新。
- 测试：store 查询、service DTO 组装（含跨 session/实例隔离与失败派生无链接场景）、集成测试覆盖两层嵌套历史懒加载。
- 前端影响：纯新增字段，向后兼容；旧客户端可忽略。
