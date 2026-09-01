## Context

现状：`internal/agent` 采用固定三角色——Confucius（唯一调度者）+ Chongzhi（写）/ Liang（读），`invoke_*` 工具在 Confucius 的 dispatch switch 中前置拦截（不进 registry），子 Agent 按次经 `SubAgentFactory` 构建隔离实例，事件经 `stream.TaggedWithRunID` 注入 run 身份（= tool_call id），`messages.run_id` 落库。模型/思考等级为 turn 级 override，统一注入全部 agent。per-request 的 MCP manager 每回合共享。

本设计将固定角色替换为「通用子 Agent + 单一 spawn_subagent」，新增历史快照续跑与全树预算。动机见 proposal.md - Why，行为契约见 specs/。

## Goals / Non-Goals

**Goals:**

- 单一通用子 Agent 实现，能力 = 通用 prompt + 派发时收窄的工具集
- 稳定实例身份 + 每派发 run 身份的双层身份模型，支撑续跑与 UI 线程聚合
- 全树 depth / concurrency / total 预算，嵌套默认关闭（depth=1 行为等价现状）
- 存量数据与事件流兼容：NULL 身份退化按 agent 名路由

**Non-Goals:**

- 不做子 Agent 间通信（mailbox / send_message / wait_agent）
- 不在 spawn 参数中暴露模型/思考等级选择（继承 turn 配置，后续变更再议）
- 不做跨会话续跑（实例快照按会话归属校验）
- 不做异步/后台派发（保持同步并行：一轮 N 个 spawn，全部返回后主循环继续）

## Decisions

### D1: 通用子 Agent 是单一参数化实现，Confucius 保持唯一根

Chongzhi/Liang 的 tool-calling 循环本已高度同构，合并为一个由 `SubAgentSpec{system_prompt, tools, max_rounds, depth}` 构建的通用实现；Confucius 保留独立的根循环（承担派发拦截、预算管理、usage 汇总）。子 Agent 在深度预算内同样拦截 `spawn_subagent` 并递归构建下一层。

替代方案：保留三个具名类型、仅改 tool schema —— 拒绝，角色画像正是本变更要移除的耦合，且三份循环逻辑会持续漂移。

### D2: spawn 工具沿用前置拦截，工具收窄在派发时构建 scoped registry

`spawn_subagent` 与现 `invoke_*` 一样是合成工具：出现在模型 tools[] JSON 中、由 dispatch 层拦截、不进 `tool.Registry`。每次派发按解析出的 `tools`（校验为父有效集子集）从父 registry 派生 scoped registry；MCP 工具沿用每回合共享的 manager，只做引用裁剪不重建连接。

替代方案：把子 Agent 注册为真 registry 工具 —— 拒绝，会引入 registry 对 agent 层的反向依赖。

### D3: 身份双层模型——instance_id 稳定、run_id 沿用 tool_call id

派发层生成短随机 instance id（如 `w-a1b2c3`）；run 身份继续取该次 tool_call id。事件注入从单字段包装升级为双字段 view（instance + run）。`messages` 表新增 nullable `agent_instance_id` 列；usage 的 `by_agent` key 取 `<name>#<短id>`（无 name 时 `subagent-<短id>`），保证可读且唯一，续跑累计同 key。

替代方案：run_id 直接复用为续跑身份 —— 拒绝，续跑是新 tool_call（新 run），复用会让「实例线程」与「单次执行」两个概念在事件层不可分，前端无法既聚合线程又区分执行。

### D4: 历史快照存独立表，快照为续跑权威，事件行仅供展示

新表 `subagent_runs`：`(id, session_id, agent_instance_id, parent_instance_id, depth, name, tools_json, status, messages_json, create_time, update_time)`。子 Agent Run 结束（成功/失败/capped）时全量 upsert `messages_json`（OpenAI chat 格式的完整 `[]Message`）。续跑读快照 + 追加新 user message。存量事件行不迁移、不参与续跑。

替代方案：从 `messages` 事件行反推 chat 历史 —— 拒绝：需要维护「事件→chat 格式」重建器，token 拼接/截断/续写折叠等边缘会持续腐蚀；且事件行（给人看）与重建结果（给模型用）双真相易漂移。快照方案以 Run 结束时的内存真相为准，写入点唯一。

快照体积防护：设快照大小上限，超限实例保留结果与身份但标记不可续跑，续跑返回 bad_args（错误文本指明快照超限）。

### D5: 全树预算控制器挂在 turn 上，经闭包链下传

每 Confucius turn 构建一个 `treeBudget{sem chan struct{}, total int64, depthBudget}`：并发槽为加权信号量（一次派发占 1 槽，被等待的父链持续占槽）；total 原子递增；深度预算决定子 Agent 的 tools[] 是否含 `spawn_subagent`。预算对象经 SubAgentFactory 闭包捕获下传，孙派发共用同一预算。

与现有 per-round token retry budget 正交：token 预算管瞬态重试成本，tree 预算管派发拓扑。

### D6: 并发等待采用 FIFO 排队 + 超时，配置校验防死锁

同步嵌套下，父等待子期间持续占槽：深度 d 的链占 d+1 槽。若 `max_concurrent <= max_depth`，兄弟链互相等槽可能死锁。两层缓解：(1) 配置校验拒绝 `max_concurrent <= max_depth`；(2) 等槽设上限（如 120s，可配），超时返回预算错误 tool result，模型可自行收敛重试或放弃。

### D7: retry 与 resume 正交，失败快照同样可续跑

现有 `ToolCallTracker` 幂等重试（执行过写操作则不自动重试）逐 run 保留；resume 是显式新派发而非重试，不占用 retry 预算。失败实例的快照（含已积累历史）同样持久化，主 Agent 可对 error 实例 resume 抢救——这正是「执行过副作用不重试」约束的正规出口。

### D8: preset 是纯配置模板，不构成角色调度

`agents.subagent.presets.<name>: {system_prompt, tools[]}`；spawn 的 `preset` 参数取默认 prompt/工具集，显式 `tools` 覆盖模板。原 Chongzhi/Liang 语义可在部署侧自行为 `writer` / `reader` 两个 preset 达成（迁移文档给出示例），但系统不内置、不特判任何 preset。

### D9: 派发纪律写进 Confucius system prompt

Confucius 的 prompt 需新增一节「何时派发」：子 Agent 看不到主对话，task prompt 必须自包含（文件路径、错误信息、决策上下文直接写入）；优先派可并行、有界、独立的读多写少任务；小任务与强串行任务自己完成；写操作分散到不同子 Agent 时注意写集不相交；capped 实例可续跑。这是三家主流实现共同沉淀的经验，属于本变更的核心交付物之一。

## Risks / Trade-offs

- [Token 成本放大：模型滥用 spawn] → max_total_per_turn + max_concurrency 硬预算；派发纪律 prompt；usage 按实例归因可观测
- [嵌套等待死锁] → D6 配置校验 + 等待超时
- [快照体积膨胀] → 大小上限 + 超限禁续跑；快照表按会话级联删除（对齐 turn_usage）
- [事件行与快照双真相漂移] → 快照为续跑唯一权威，事件行仅展示职责；不提供从事件行重建续跑历史的路径
- [前端迁移成本：聚合键变化] → 双字段同时下发（instance + run），旧键 NULL 兼容存量；api/openapi.yaml 同步契约
- [模型面破坏性：invoke_* 消失] → 单版本硬切（后端与前端同仓发布）；config 校验给出明确迁移错误信息

## Migration Plan

1. DB migration（顺序命名）：`messages` 加 nullable `agent_instance_id`；新建 `subagent_runs` 表（会话删除级联）。
2. config：`config.example.yaml` 增 `agents.subagent`（含预算与 presets 示例）；加载器拒绝 `agents.chongzhi` / `agents.liang` 残留段并指向迁移文档。
3. 存量行为：旧行 NULL 身份按 agent 名路由（既有退化路径）；`AgentChongzhi/AgentLiang` 常量仅保留给存量行展示解码。
4. 回滚：代码回退 + 还原配置快照；新增列/表对旧代码无影响（不读写即忽略）；`subagent_runs` 数据可保留待重升级复用。

## Open Questions

- spawn 是否暴露 per-instance 模型/思考等级参数（当前继承 turn 配置）——后续变更
- 快照是否压缩存储（先 JSON 明文，观察体积再定）
- instance id 具体格式（ulid / nanoid / 自增短码）——实现期定，不影响契约
