## Context

当前事件模型中子 agent 的唯一身份是 `StreamEvent.Agent`（显示名），而 `orchestratorFactory.Build` 每 turn 只构造一个 Chongzhi、一个 Liang 实例。当 Confucius 一轮发出 N 个同名 `invoke_*` 时（`dispatchToolCalls` 的 errgroup 并发执行）：

1. **流身份缺失**：N 路 token/reasoning 事件都打 "Chongzhi" 标签进同一个 hub，按到达顺序交错；`MergeEvents`（`event_mapper.go`）只合并相邻同 agent 事件，交错流持久化成交替碎片行；前端 `findSegmentIndex` 按名字找活动段，三路文本拼进同一段。
2. **共享实例状态竞争**：`Chongzhi.Run` 入口在 `runMu` 下把 `executedToolThisRun`/`hitCapThisRun` 清零，并发 Run 之间互相覆盖——Run 2 的入口清掉 Run 1 刚置位的副作用标志（重试幂等失效），Run 1 的工具执行让 Confucius 对 Run 2 误判已有副作用（该重试的不重试）；`round_capped` 传播同理串读。

实证：session `01a01457-f5dc-73ae-8079-4aa151c8d2ec`（一轮 3× `invoke_chongzhi`，messages idx 20–30 中两路文本句子互相穿插、idx 26 两个 run 的句子被粘连）。

约束：子 agent 事件当前由子 agent 自己 `hub.SendCtx` 发出（agent 层不知道调用方是谁）；`messages` 表已有确定性 `client_msg_id` 幂等键体系（`{trace_id}:{msg_index}`）与 Redis 读缓存路径；前端在兄弟仓库 blowball-frontend，契约以本仓库 `api/openapi.yaml` 为源。

## Goals / Non-Goals

**Goals:**
- 并发同名子 agent 调用（含 token/reasoning/tool_call/tool_result/lifecycle 全事件）端到端可区分：SSE、`messages` 表、前端流式分段与历史重载分段。
- 每次调用的 `executedToolThisRun`/`hitCapThisRun` 判定独立，重试幂等与 `round_capped` 传播在并发下正确。
- 保留并行执行（不做同名串行化）。
- 子 agent（Chongzhi/Liang）代码零改动或近零改动。

**Non-Goals:**
- 不做同名串行化（方案 A，明确否决——牺牲并行且会被本方案取代）。
- 不改变 `MergeEvents` 的相邻合并规则本身（交错行落库形态不变，靠 run 身份事后归属；见 Decisions D4 的取舍）。
- 不处理 glm-5.2 `"..."` 占位符 token（另案）。
- 不给 Confucius 自身的事件加 run 身份（顶层回合天然单线程；Confucius 的 tool_call/tool_result 已带 `meta.tool_call_id`）。
- 不改 usage 归因（`by_agent` 已按显示名聚合，保持现状）。

## Decisions

**D1. run 身份 = invoke tool_call 的 `tc.ID`，经包装 hub 注入，agent 层零感知。**
新建 `runTaggerHub`（`internal/stream/` 或 `internal/agent/`）：持有内层 `*Hub` 与 `runID`，实现同样的 `Send/SendCtx` 接口，发送前向 `e.Meta["parent_tool_call_id"] = runID` 写入（`Meta` 为 nil 时初始化；已有值不覆盖——invoke 结果事件由 Confucius 直接发，不经此 hub）。`dispatchOne` 在进入 `dispatchSubAgent` 前用 `&runTaggerHub{inner: hub, runID: tc.ID}` 包一层传给 `sub.Run`。
*备选否决*：(a) 给 `Agent` 接口加 run 参数——侵入三个 agent 与全部测试；(b) 用 context 传 run_id 让 agent 自己读——agent 层每个 Send 点都要取 ctx，改动面更大且易漏。包装 hub 把标注收敛在一个切面，`Close`/`Done`/`Events` 直接透传内层（tagger 不拥有生命周期）。
*命名*：字段名 `parent_tool_call_id` 而非 `run_id`——它是现成的、全局唯一的关联键（前端已有 `meta.tool_call_id` 概念），且天然表达了"谁派生的这次 run"。

**D2. 每次 invoke 新建子 agent 实例（方案 B）。**
`orchestratorFactory.Build` 不再把 Chongzhi/Liang 单例塞进 `subAgents` map，改为把"构造函数"交给 Confucius：`subAgents` 值类型从 `Agent` 改为 `func() (Agent, error)`（或预构建的 `*agentBlueprint`，封装 cfg+client+reg 的构建入参）。`dispatchSubAgent` 每次调用现 build 一个实例、用完即弃——`executedToolThisRun`/`hitCapThisRun` 变成事实上的 per-run 状态，`runMu` 可保留（防御性）但不再有跨 Run 语义。
*构建成本*：每次 invoke 重建的内容是 cfg 克隆、toolsJSON 渲染（`json.Marshal` 一次）、per-request registry 重组（xizhi 闭包绑定 workspace root）。对一轮 2–3 个 invoke 可忽略；若 profiling 证明 registry 重组贵，可退化为"共享只读部分（registry/toolsJSON）+ 按调用克隆可变壳"，正确性目标不变。**只读部分（registry、client、cfg）明确允许共享**——需要保证的只是可变 run 状态隔离。
*备选否决*：把两个标志改成 `map[runID]bool` 挂在共享实例上——引入 run 生命周期清理问题（何时删 key），不如实例即弃干净。
*MCP manager 注意*：per-turn 的 `mcp.Manager` 与 `TurnCloser` 仍由 `Build` 创建一次、整 turn 共享（连接复用语义不变），只是不再随子 agent 实例一起单例化——构建函数捕获同一个 manager。

**D3. 持久化：`messages` 加 `run_id CHAR(64) NULL` 列。**
新迁移 `migrations/014_subagent_run_id.sql`（存量库手动应用，沿用 012/013 惯例）。`MessageFromEvent` 从 `e.Meta["parent_tool_call_id"]` 读出写入；`model.Message` 加字段；mysql/redis 读写路径透传（Redis `msgs:` 缓存的 JSON 行多一个字段，无需结构迁移）。NULL = 非子 agent 事件（Confucius 顶层、用户行）。
*排序不变*：行仍按 `(msg_time, msg_index)` 到达序存储——`msg_index` 生成、`client_msg_id` 确定性幂等体系全部不动。交错行靠 `run_id` 事后归属，这是有意取舍（见 D4）。

**D4. 归属与展示：按 `(agent, run_id)` 分段，不在落库前重排。**
- 后端重建（`message_reconstruct.go` / `MessagesToAgentMessagesIndexed`）：遇带 `run_id` 的行按 `(agent, run_id)` 分组重放，组内保持到达序——交错行在喂给 LLM 前重组为连贯的 per-run 片段；`agent_start`/`agent_end` 语义按组配对。
- 前端（blowball-frontend）：`streamingSegments` 的路由键从 `agent` 改为 `agent + run_id`（`findSegmentIndex` 匹配复合键；`meta.parent_tool_call_id` 缺失时视 run_id 为空——单 agent 回合退化为现行为）；`groupMessages` 同样按复合键切块。openapi 契约在 SSE 事件 schema 上补 `meta.parent_tool_call_id`（可选字段），前端重新生成类型。
*备选否决*：落库前按 run 重排（重写 `msg_index`）——会破坏 `client_msg_id` 确定性幂等与 mid-turn flush 的一致性契约，收益仅是省掉前端分组逻辑，不值。

**D5. SSE 载荷向后兼容。**
`parent_tool_call_id` 是 `meta` 里的新增可选键；旧前端忽略未知键，无破坏。`done` 事件、usage 结构不变。

## Risks / Trade-offs

- [每 invoke 重建子 agent 的构建开销] → 量级为一次 registry 组装 + JSON 渲染；先按简单实现，benchmark 不达标再退化到"共享只读 + 克隆可变壳"（D2 已留路径）。
- [交错行落库、事后归属意味着原始行序仍"乱"] → 有意取舍：保住 `msg_index`/幂等体系；任何消费方按 `run_id` 分组即得连贯序。若未来要物理重排，需单独评审对 mid-turn flush 的影响。
- [前端改动跨仓库] → 以 `api/openapi.yaml` 为契约源同步：本仓库先合契约，前端仓库随后重新生成类型并改分段键；旧前端在新后端下行为不变（忽略新键），可灰度。
- [`runTaggerHub` 忘记透传某个 Hub 方法导致事件丢失] → 让 tagger 直接内嵌 `*Hub`（只覆写 `Send/SendCtx`），编译期保证其余方法透传；单测断言每个发出的子 agent 事件都带 `parent_tool_call_id`。
- [重建按 run 分组后，tool_call/tool_result 配对逻辑复杂化] → 现有配对按 `tool_call_id`，与 run 分组正交；单测覆盖"交错行 → 分组重放 → 配对不变"。
- [迁移 014 在存量库漏用导致 run_id 全 NULL] → 与 012/013 同惯例：变更说明标注手动应用；代码路径对 NULL 容忍（等价于今天的无身份行为）。

## Migration Plan

1. 后端合入（含迁移 014）：新回合的子 agent 事件开始携带并持久化 `run_id`；旧行 NULL。
2. 存量库手动执行 `migrations/014_subagent_run_id.sql`（与 012/013 相同的运维惯例）。
3. 前端仓库随后合入分段键改造（重新生成 openapi 类型）；前后端可独立部署——中间态下旧前端忽略新键，行为等同今天。
4. 回滚：后端回退版本即可，`run_id` 列留存无害（NULL 语义 = 现行为）。

## Open Questions

- `runTaggerHub` 放 `internal/stream/`（作为 Hub 的通用装饰器）还是 `internal/agent/`（dispatch 专用）——倾向前者，倾向在实现时按包依赖最小定。
- 重建分组是否需要给 LLM 上下文里的 per-run 片段加"调用 #N（任务摘要）"类分隔提示——先不加，保持重建输出与单 run 形态一致；若实测并行回合的下一轮上下文理解差，再评估。
