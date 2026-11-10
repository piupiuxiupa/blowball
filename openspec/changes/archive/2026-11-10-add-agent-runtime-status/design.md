## Context

当前 turn executor 通过 `req.Content + prompt.RenderStatus()` 把时间状态拼进用户正文，随后同一字符串进入 send-time 持久化和标题生成。Confucius/SubAgent 的 `max_rounds` 是最外层工具循环上限，不是单个工具调用次数；达到上限后已有 tool-disabled wrap-up，但此前没有逐步预算提示。

供应商提示词缓存是临时的，且只复用请求前缀。若每轮替换或删除一条动态状态，上一次包含该状态的完整请求就不能成为下一次请求的前缀。要保留最近一次完整请求前缀，旧状态必须保持不可变并留在原位置，新内容和新状态只能继续追加。

MySQL 是会话事实来源，现有 Redis `msgs:{sid}` 是其消息行读缓存。运行时状态是可再生的提示信息，不需要进入 MySQL，但在活跃会话的 Redis 生命周期内保留它们，可以让 Redis 重放与供应商临时缓存大致对齐。Redis miss、重启或驱逐仍可能提前造成缓存断点，因此该设计只能优化命中率，不能成为正确性依赖。

压缩工具台账是另一项独立但相关的上下文保留需求：当前 `BuildToolActivity` 只按 `call.Function.Name` 聚合，导致 `mcp_call` 的 server/tool 分布丢失。

## Goals / Non-Goals

**Goals:**

- 在活跃 root turn 内保持“真实消息 + 状态消息”的 append-only 模型 transcript，使上一次完整请求在 system/tools 不变时成为下一次请求的前缀。
- 在 Redis 中独立保存 root 状态及其 canonical 锚点，命中后跨用户 turn 逐 byte 重放；MySQL、SSE 和前端历史保持干净。
- Redis miss 或锚点校验失败时，从 MySQL 干净历史开始新缓存纪元，生成新状态，不影响对话正确性。
- 让 Confucius 和每个 SubAgent 按自身 `max_rounds` 看到准确的工具轮次倒计时，提前安排总结。
- 将时间/上下文状态与用户原文分离，并把当前所有状态及新状态自身纳入请求预算。
- 保持瞬时重试、length continuation、structured output、live overlay、压缩和原始 LLM 请求捕获的既有契约。
- 在压缩 Tool Activity 中保留 `mcp_call` 的 server/tool 嵌套统计。

**Non-Goals:**

- 不新增实际工具调用次数硬上限或 `max_tool_calls` 配置。
- 不改变 `max_rounds`、round-cap wrap-up、重试预算或 continuation 的计数规则。
- 不新增 SSE 事件、HTTP 字段、MySQL 表/列或特性开关。
- 不要求 Redis 与供应商缓存 TTL 完全一致，不保证每次请求都命中缓存。
- 不把状态写入现有 `msgs:{sid}` 消息行缓存，不让它参与真实用户 ordinal、标题或历史 API。
- 不为 SubAgent 增加跨 turn 模型续跑；其状态只需在单次 Run 内 append-only。
- 不向标题、压缩摘要、webfetch digest 等旁路 LLM 调用注入运行时状态。
- 不清洗或迁移已经持久化的旧式内联 `<current_status>`，不从旧 checkpoint 猜测 MCP 历史明细。

## Decisions

### D1: 区分 durable canonical 与 active model transcript

**durable canonical** 只包含真实用户、assistant、tool 及既有协议消息，是 MySQL、历史重建和压缩的事实来源。**active model transcript** 是 durable canonical 加上 cache-only synthetic user 状态，在 root 活跃 turn 内只追加、不改写：

```text
上一请求 = system + active transcript
下一请求 = system + active transcript + 新 assistant/tool + 新状态
```

因此，只要 system prompt 与 tools 未变化，上一请求的完整 messages 是下一请求的前缀。状态一旦用于某个逻辑请求便不可变；后续余量或倒计时通过新消息表达，最新状态优先。

`current_status` 放在真实当前用户消息之前，避免 `collectLiveMessages` 把状态识别为当前用户输入。`tool_budget` 放在该轮全部 tool results 之后，不插入任何 tool-call 配对之间。

**备选方案：** 每轮替换请求尾部 overlay 虽节省 token，但上次完整请求不再是下次前缀，拒绝。把状态写入 MySQL 虽可长期重放，但会污染历史并长期占用上下文，且供应商缓存过期后没有收益，拒绝。

### D2: 使用独立 Redis 状态日志和 canonical 锚点

新增 session-scoped `runtime_status:{session_id}` Redis LIST；名称可沿用项目 key 风格微调，但必须与 `msgs:{sid}` 分离。每项包含：

- 确定性 `status_id`；
- `kind`（`current_status` / `tool_budget`）与不可变渲染内容；
- 单调 `seq`；
- 锚点类型和值：`before_client_msg_id` 或 `after_tool_call_id`。

初始 `current_status` 锚定在当前用户 `{trace_id}:0` 之前；`tool_budget` 锚定在该 assistant 响应最后一个 tool_call 对应结果之后。tool_call id 在工具执行时已经存在，无需等待 round-final MySQL 批写。

恢复时先按现有路径重建 durable agent messages，再按锚点合并状态：before-user 状态插到对应用户消息前，after-tool 状态插到对应 `role=tool` 消息后；同一锚点按 `seq` 排序。现有 `MessagesToAgentMessagesIndexed` 的 row attribution 可用于定位用户行，tool 消息直接按 `ToolCallID` 定位。

状态日志采用**完整校验**：所有状态的锚点均能在恢复后的 canonical transcript 中找到时才重放；任一锚点缺失、顺序损坏或内容无法解析，就作废整份状态日志、best-effort 清除并开启新缓存纪元。这样不会把半份状态拼到错误位置。`msgs:{sid}` 是否命中可以独立决定，但状态只有在锚点与 MySQL 重建结果一致时才使用。

`DeleteSession` 同时删除该 key；每次状态写入刷新与现有消息缓存同族的 TTL。TTL 不要求等于供应商 TTL，miss 仅表示失去一次缓存优化机会。

### D3: 状态由 agent 执行路径追加，不由消息持久化器生成

root 在首次模型请求前生成一次 `current_status`，并立即在 active transcript 与 Redis 状态日志中以同一 `status_id` 记录。每个工具派发组完成后、下一逻辑请求前生成 `tool_budget` 并追加。无需等待 incremental round persister；若进程崩溃导致锚点 tool result 未持久化，下次完整校验会丢弃该状态日志。

`status_id` 由 trace、root/round、工具组和状态类型确定。`RuntimeStatusSeq` 是独立的日志序号：Redis 重放消息携带原条目的真实 `Seq`，root 下一状态取重放消息中的 `max(Seq)+1`，不能用状态条数推断。压缩后内存 current_status 为 Seq 1、后续 tool_budget 为 Seq 2 时，下一 turn 必须续为 Seq 3。瞬时重试复用已构造请求，不再次追加。incremental persister、mid-turn flush 和 terminal batch 只处理 durable canonical，不读取或重写 Redis 状态日志，因此不会经多个持久化路径重复追加。

Redis 写失败只记录 WARN：当前内存 turn 继续使用 append-only 状态，后续 turn 按新缓存纪元降级。状态缓存永远不阻塞或失败一个 turn。

### D4: 倒计时来自最外层循环，SubAgent 只在内存中追加

完成第 `i` 个 1 基工具循环后：

```text
rounds_used      = i
max_rounds       = 当前 agent 实例的 maxRounds
rounds_remaining = max(maxRounds - i, 0)
```

同一响应的多个并行 calls 只对应一个 `i`。length continuation 和瞬时重试不推进 `i`；continuation 中若实际执行了新的工具组，可以追加同轮次的新状态，但必须使用新的工具组序号。达到 `i == maxRounds` 时，wrap-up 看到零剩余状态与既有指令，tools 仍被省略。

SubAgent 每次 dispatch 都是新实例且没有模型驱动跨 turn resume，因此只在自身 active transcript 中 append-only，不写 root Redis 状态日志。`lastMessages` 和 `subagent_runs.messages_json` 的持久化投影必须排除 synthetic 状态；重试同一实例时也不得把旧状态误当成父级 durable 输入。

### D5: 用户原文与状态计算分离

`internal/turn/executor.go` 不再调用 `RenderStatus` 拼接正文，`content` 直接等于 `req.Content`；同一原文进入真实用户消息、send-time `PersistInfo.UserContent` 和标题流程。

`internal/prompt` 只提供接收 plain data 的固定格式渲染，不自行读取 turn、模型或预算。root 在 system prompt/tools 构建完成后捕获本 turn 固定 RFC3339 时间戳并生成独立 `current_status`。旧式已入库的内联状态继续按原字节读取。

`llm_raw_log` 不做特殊过滤，继续记录实际发送的完整请求。“状态不持久化”特指不进入 MySQL 会话数据、SubAgent transcripts、SSE 和前端历史，而不是排除原始请求可观测性记录。

### D6: 上下文状态包含全部现存状态，并用保守测量消除自引用

`current_status` 至少包含 `estimated: true`、`estimated_input_tokens`、`input_budget_tokens` 和 `estimated_remaining_tokens`。输入预算继续使用 `NewRequestBudget(max_context_tokens, max_completion_tokens)`，剩余量 clamp 到零，不能使用累计 `turn_usage.total_tokens`。

首次状态估算覆盖 system、tools、durable history、Redis 重放的旧状态、新 current_status 和真实当前用户消息。为避免数字改变状态自身长度，使用字段形状相同、数字采用保守固定宽度的测量消息先估算，再渲染实际短格式状态；实际余量不得大于最终完整请求的预算差值。

后续 preflight 必须包含 active transcript 中全部旧状态及即将追加的状态。`compactMidTurn` 在 `RequestOverheadTokens` 中额外加入 `EstimateCurrentStatusOverhead`，使候选选择在接受 retained tail 之前就预留一条保守 current_status；不得等候选验收后再无预算地追加。compaction 成功后会清除旧状态，因此压缩候选只需保留压缩后新状态的开销，不必为将被丢弃的历史状态预留空间。无有效正预算时只渲染时间戳，省略 context 数值。

### D7: Redis miss 与 compaction 都开启新缓存纪元

以下情况允许且必须显式降级：

- Redis 状态 key 不存在、过期或读取失败；
- 任一状态锚点在 durable 重建中不存在；
- compaction 实际改写了 canonical projection，并由 `resetRuntimeStatusAfterCompaction` 切换缓存纪元；
- system prompt/tools 或模型选择变化导致旧前缀自然失效（无需清除状态，但不宣称缓存可命中）。

永久性的 `ContextCompacted` 标记本身 SHALL NOT 触发清理。相同 checkpoint 的 stitched projection 必须保留 row attribution 并正常重放状态；只有实际 compaction replacement 才清理。miss 时从 MySQL canonical 历史生成新的 current_status，旧状态不参与请求。compaction 输入本来就不含 synthetic 状态；成功后清空 session 的旧状态 key，在新的 compacted active transcript 末尾追加最新状态。压缩摘要与 Tool Activity 不记录这些状态。

### D8: 协议判断使用 durable/canonical 投影

- `terminalResponseFormat` 必须忽略尾部 synthetic 状态，仍根据最后一个真实 tool result 判断 SubAgent 终轮。
- `collectLiveMessages`、附件内联和 live overlay 只把真实用户消息作为当前输入；current_status 位于其前。
- mid-turn/preflight compaction 使用 MySQL durable 内容，不摘要 runtime 状态。
- continuation 保留既有 canonical 脚手架，新状态只追加在完整 tool 应答之后。
- tool result、SSE、消息批写与 SubAgent run transcript 通过明确类型/投影排除状态，不能依赖内容字符串偶然过滤。

### D9: MCP 明细作为 ToolActivity 的可选嵌套台账

压缩提示词已经禁止摘要模型输出 Tool Activity，因此不修改提示词。`BuildToolActivity` 处理 `mcp_call` 时额外解析原始 arguments 的 `server` 与 `tool`：

```text
mcp_call: parent calls/success/errors（保持现有字段与回滚兼容）
└── server
    └── remote tool: calls/success/errors（新增可选 JSON 明细）
```

顶层仍按宿主工具名合并，`TotalToolActivityCalls` 只累加顶层，避免父子重复计数。子项按 `(server, tool)` 合并，成功/失败沿用同一次调用的结果分类；普通工具、其他 `mcp_*` 管理工具和 operator MCP 代理工具口径不变。

参数无效或旧 checkpoint 只有父级总数时，调用保留在未归因余量，不猜测名称。再次压缩只累加可识别的新明细，渲染可显示 `unattributed = parent - attributed`。`tool_activity_json` 只增加 optional 字段，旧二进制仍可读父级统计。

子项不复制父级 examples，所有 server/tool 计数必须保留，不能用 Top N 静默丢弃；预算不足时优先缩减 retained tail 和 examples，仍不可拟合则沿用显式 uncompactable 结果。

## Risks / Trade-offs

- **append-only 状态会逐步增加上下文和 Redis 体积** → 使用固定短格式，compaction 时清理旧状态；所有状态计入预算，不通过修改旧状态节省空间。
- **Redis 可能比供应商缓存更早 miss** → 只损失一次缓存优化，完整锚点校验失败后从 MySQL 安全降级，不影响正确性。
- **状态日志与消息缓存可能部分写入或短暂不一致** → 采用全量锚点校验，任一失败就作废整份状态日志，不做部分拼接。
- **多个执行/持久化路径可能重复追加状态** → 状态只由 agent 逻辑请求路径生成，使用确定性 status_id；持久化器不管理状态，重试不追加。
- **旧倒计时会让模型看到历史余量** → 每条状态带轮次，格式明确最新状态优先，且不能修改旧内容。
- **system/tools 变化仍会破坏前缀** → 只保证状态本身不额外制造断点，不承诺跨模型、跨工具配置缓存命中。
- **MCP server/tool 组合基数可能使台账增长** → 子项只保留必要计数，预算规划计入完整明细；不用 Top N 丢统计，极端情况下缩减其他内容或显式 uncompactable。

## Migration Plan

1. 先加入 Redis 状态 append/replay/锚点校验、用户原文、倒计时及 MCP 嵌套台账测试，再接入执行路径。
2. 无需 MySQL migration、配置变更或前端同步；新增 Redis key 可随正常后端发布启用，旧 `msgs:{sid}` 数据格式不变。
3. 发布后通过连续 `llm_raw_log` 请求验证上一请求是否为下一请求前缀，并观察 Redis miss、compaction 清理和上下文预算日志。
4. 通过新压缩记录及再次压缩结果核对每个 server/tool 的计数守恒、旧记录未归因余量和顶层总调用数不重复。
5. 回滚时部署前一版本即可：旧代码不读取新 Redis key，MySQL 从未包含新状态；可 best-effort 清理该 key，无数据反向迁移。

## Open Questions

无。实际单个工具调用次数硬上限不在本变更范围内。Redis 与供应商缓存 TTL 不做强绑定，缓存命中收益以发布后 gateway 实测为准。
