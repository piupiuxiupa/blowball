## Why

当前 `subagent_runs` 以 `(session_id, agent_instance_id)` 为唯一键保存实例最新完整快照：同一实例每次续跑都会覆盖上一轮快照，无法按 run 查看或懒加载历史执行详情。前端历史页只能从 `messages` 的子 Agent token/reasoning 事件碎片中还原长内容，导致长会话加载、拼接和渲染成本过高。

## What Changes

- 将子 Agent 持久化历史从“实例最新完整快照”改为“实例元数据 + 每次 run 的消息 delta”：
  - `agent_instance_id` 继续表示跨续跑稳定的实例线程；
  - `run_id` 表示一次派发/执行，来自父 `spawn_subagent` tool call id；
  - 同一实例的多个 run 通过 `previous_run_id` / `run_no` 组成线性链；
  - 每个 run 行的 `messages_json` 只保存本次执行新增的 user/assistant/tool 消息，不重复保存旧上下文。
- 引入实例级元数据存储，保存稳定实例身份、父实例、深度、名称、system prompt、有效工具与最新 run 指针。
- 续跑时按 run 链拼接 delta 得到完整模型上下文，再追加新任务；链路损坏、跨会话目标、不可读快照或超限上下文按 bad_args/不可续跑处理。
- 新增子 Agent run 详情懒加载 API：
  - 列出某实例的所有 run 元数据；
  - 获取某次 run 的详情；
  - 后端将内部 chat 消息转换成稳定的前端 transcript DTO，不原样暴露 `messages_json`、system prompt、`tools_json` 或 `resume_eligible`。
- 保持 `messages` 事件流、SSE 与 Redis run stream 的现有流式语义不变；本变更不把长期历史事件改成合成大消息。
- 迁移既有 `subagent_runs` 数据：保留 legacy 完整快照语义并回填可得的 run 身份，新数据使用 delta 语义；不伪造已被覆盖的旧 run。
- 更新 `api/openapi.yaml`，补齐新端点、错误与 DTO 契约。

## Capabilities

### New Capabilities

- `subagent-run-transcripts`: 定义子 Agent per-run delta 持久化、run 链拼接、run 列表/详情懒加载 API、所有权校验与安全 DTO 转换。

### Modified Capabilities

- `dynamic-subagents`: “Resume with persisted history” 的存储要求从每个实例保存一份最新完整快照，改为实例元数据加 per-run delta，并通过拼接 run 链恢复完整续跑上下文。

## Impact

- **数据库迁移**：
  - 拆分或改造 `subagent_runs`，引入实例级元数据与 per-run delta 行；
  - 新增/调整唯一键与索引：`(session_id, run_id)` 唯一，`(session_id, agent_instance_id, run_no)` 唯一；
  - 回填 legacy run 身份并标记 snapshot kind，保留既有数据可续跑。
- **Agent 派发与续跑**：`internal/agent/dynamic.go` 的 snapshot load/save、resume 身份链、delta 切片与上下文大小判断。
- **存储层**：`internal/model`、`internal/store/mysql` 的子 Agent 实例/run 读写、事务性追加与 latest run 指针维护。
- **HTTP API**：新增 session 作用域的子 Agent run 列表与详情路由、DTO、所有权校验和错误语义。
- **文档**：更新 `api/openapi.yaml` 与 OpenSpec 规格。
- **兼容性**：不改变 `spawn_subagent` 参数、`agent_instance_id` 对外语义、`messages` 表事件流或 SSE 协议；已存在的 legacy 实例必须仍可按旧完整快照续跑。
