## Context

动态子 Agent 的读取链路分两段：

- **live**：dispatch 层通过 `stream.TaggedWithAgentRun` 给每个子 Agent 的事件打 `meta.agent_instance_id` / `meta.parent_tool_call_id`（first-stamp-wins，嵌套不丢）；done 事件的 `usage.meta.sub_agent_invocations` 已含全量调用树（含 `parent_instance_id`）。
- **durable**：`messages` 表只回填 **Confucius 顶层** spawn tool_call 占位行的子实例 id（`round_persist.go` 的 `spawnInstances` 映射）；子 Agent 自身事件一律不落 messages，其 transcript 存 `subagent_runs.messages_json`。嵌套 spawn 时，孙实例的 run 行虽已持久化（`parent_instance_id` 指向子实例），但孙实例 id 在任何读取响应中都不出现——`SubAgentRunSummary`/`SubAgentRunDetail` DTO 丢弃了 `parent_instance_id`，transcript DTO 的 `SubAgentToolCall` 只有 `id/name/arguments`，无法把子 Agent run 内的 `spawn_subagent` tool call 关联到孙实例。

既有约束：run 详情只暴露终态快照（执行中 404）；读取 API 必须校验 session 归属且不区分 404 成因；DTO 不得泄露 `messages_json`/system prompt/内部计数器。

## Goals / Non-Goals

**Goals:**

- 历史加载时，前端从任意一层的 run 详情出发，都能拿到其内部嵌套 spawn 对应的子实例 id，从而递归懒加载整棵已终止的子 Agent 树。
- run 列表/详情响应可表达实例在树中的父节点。
- 纯读侧扩展：不改写入路径、表结构、SSE 事件；不加新路由。
- 向后兼容：全部为新增可选字段。

**Non-Goals:**

- 不新增 session 级「整棵子 Agent 树」端点（方案 A，留待真实前端需求出现后再评估）。
- 不持久化 done 事件的 invocation 列表（方案 C，涉及写路径与存储，收益不足）。
- 不解决 legacy 数据中 `run_id` 被 backfill 为 `legacy:<instance>` 导致的关联缺失（自然降级为无链接，见 Risks）。
- 不改变 live SSE 的嵌套事件归因（现状已正确）。

## Decisions

### D1：链接数据源 = `subagent_runs` 既有列，不新增存储

孙实例 B 的 run 行天然携带 `(session_id, parent_instance_id=A, run_id=call_B, agent_instance_id=B, name=label_B)`，其中 `run_id` 就是父 A transcript 里 `spawn_subagent` tool call 的 id。返回 A 的 run 详情时，查一次 `WHERE session_id = ? AND parent_instance_id = ?` 的子 run 行，在内存建 `run_id → (child_instance_id, child_name)` 映射，再遍历 transcript 给 `spawn_subagent` tool call 项注链接。

**备选（否决）**：按每个 tool call 做 `(session_id, run_id)` 点查——命中现有唯一键、无扫描，但引入 N+1 查询；一次父级批量查询在 transcript 含多个 spawn 时更优。会话内子 run 行数受 per-turn 预算约束，`(session_id, …)` 前缀索引扫描量可接受；不为此新增 `(session_id, parent_instance_id)` 索引（无迁移目标）。

### D2：DTO 扩展为最小平面字段

- `SubAgentRunSummary` / `SubAgentRunDetail` 新增 `parent_instance_id`（`omitempty`，根派生为空）。
- `SubAgentToolCall` 新增 `sub_agent_instance_id`、`sub_agent_name`（均 `omitempty`）。仅当 tool call 是 `spawn_subagent` 且查到终态子 run 行时填充；普通 tool call 序列化形状不变。

**备选（否决）**：嵌套对象 `sub_agent: {...}`——与现有平面 DTO 风格不一致，且旧前端对未知嵌套对象的兼容性并不更好。

### D3：查询与归属校验的顺序

store 新增 `ListSubAgentChildRuns(ctx, sessionID, parentInstanceID) ([]model.SubAgentRun, error)`（只选 DTO 所需列或复用既有行结构）。service 层沿用现状：先 `GetSessionByID` 校验归属 → `GetSubAgentRun` 取父 run → 再查子链接。子查询始终以 `session_id + parent_instance_id` 双条件作用域，绝不以裸 `run_id` 跨 session 匹配；父实例已被归属校验覆盖，子行必属同一 session，无越权面。

### D4：失败/未终态派生的语义

- bad_args、预算耗尽等**未创建实例**的失败 spawn：无子 run 行 → tool call 不带链接（前端按普通失败 tool call 渲染）。
- 实例已创建但 status=error：子 run 行存在 → 带链接；子 transcript 的 status/error 内容由既有 detail API 表达。
- 子 Agent **仍在执行**：无终态行 → 暂无链接；终态落库后（重刷/重取详情）链接出现。这与 run 详情「只读终态」的既有契约一致，live 展示仍走 SSE。

## Risks / Trade-offs

- [legacy 行 `run_id` 为 backfill 值，无法与父 transcript 的真实 tool_call id 匹配] → 该 tool call 自然无链接，行为等价于改动前；不伪造关联。
- [run 详情多一次 session 前缀扫描] → 子 run 行数受 `max_total_per_turn` 与会话规模约束；如后续出现大会话热点，再评估补 `(session_id, parent_instance_id)` 索引（独立小迁移）。
- [同一父实例多个 run（旧 resume 模型）都查回子行] → `(session_id, run_id)` 唯一保证映射无歧义；当前 fresh-dispatch 模型下每实例仅 run_no=1。
- [前端递归懒加载放大请求数] → 每层一次 detail；深度受 `max_depth` 约束，默认 1 时行为与现状完全一致。

## Migration Plan

无数据库迁移、无配置变更、无新路由。部署即生效；回滚二进制即回滚（响应字段消失，新增字段均为可选，旧/新客户端互不影响）。

## Open Questions

（无——方案 B 的契约已在探索阶段与现状代码逐点核对。）
