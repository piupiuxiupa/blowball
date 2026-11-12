# agent-runtime-status Specification

## Purpose

定义 agent 运行时状态的 cache-only 表达与恢复：以独立、不可变、append-only 的 synthetic user 消息向模型提供当前时间、上下文余量与工具轮次预算，同时保持 durable canonical、用户可见记录和持久化前缀稳定。

## Requirements

### Requirement: Runtime status is append-only within the active model transcript
系统 SHALL 区分只含真实协议消息的 durable canonical 与包含 cache-only synthetic user 状态的 active model transcript。root Confucius 的 active transcript 在活跃 turn 内 SHALL 只追加、不修改、不删除已经发送过的状态；后续上下文余量或工具轮次变化 SHALL 通过新状态消息表达，最新状态优先。

`current_status` SHALL 位于真实当前用户消息之前，`tool_budget` SHALL 位于对应工具组全部 `role="tool"` 结果之后。状态不得插入 assistant tool_calls 与其结果之间。system prompt 与 tools 不变时，上一次完整模型请求的消息序列 SHALL 是下一次请求消息序列的逐 byte 前缀；新增 assistant/tool 与最新状态只能接在该前缀之后。

#### Scenario: 初始时间状态与用户原文分离
- **WHEN** root 为一个用户 turn 构造首次模型请求
- **THEN** 请求在既有历史后依次包含独立 `<current_status>` 和用户原文消息
- **AND THEN** 两部分不被拼进同一条消息，状态位于真实用户消息之前

#### Scenario: 工具轮次保留上一完整请求前缀
- **WHEN** 第 N 次请求后产生 assistant/tool 新内容并追加第 N+1 轮状态
- **THEN** 第 N+1 次请求以第 N 次请求的完整消息序列开头
- **AND THEN** 旧时间状态和旧倒计时保持原内容、原顺序，不被新数值替换

#### Scenario: 并行工具配对不被状态打断
- **WHEN** 一个 assistant 响应包含多个并行 tool calls
- **THEN** 所有对应 tool results 先按既有协议顺序进入 active transcript
- **AND THEN** 新 `tool_budget` 只出现在完整结果组之后

### Requirement: Redis replays immutable root status by canonical anchors
系统 SHALL 使用独立于 `msgs:{session_id}` 的 session-scoped Redis 状态日志保存 root 状态。每项 SHALL 包含确定性 `status_id`、不可变内容、单调序号和 canonical 锚点。`current_status` SHALL 以 `before_client_msg_id` 锚定真实用户消息之前，`tool_budget` SHALL 以 `after_tool_call_id` 锚定对应工具组最后一条 tool result 之后。现有 `msgs:{sid}` SHALL 继续只表示 MySQL 消息行缓存。

恢复时系统 SHALL 先重建 durable canonical，再按原内容、原顺序和原锚点合并状态。重放消息 SHALL 携带原条目的真实 `Seq`，后续新状态序号 SHALL 从该日志最大 `Seq` 续接，而不是从状态条数推断。只有全部状态的锚点均存在且条目可解析、顺序有效时，状态日志才可重放；任一校验失败 SHALL 作废整份状态日志并开启新缓存纪元，不得部分插入或猜测位置。状态 key SHALL 使用现有消息缓存 TTL 家族，并在 session 删除时清理。

锚点有效性 SHALL 包含协议位置，而不仅是 ID 存在：`after_tool_call_id` SHALL 唯一对应一个完整 assistant 工具单元的最后一条 tool result；该单元声明的所有 tool_calls SHALL 已有连续、匹配的结果。位于组中间、孤立结果或未完成工具组中的 ID SHALL NOT 作为状态锚点。`before_client_msg_id` SHALL 唯一对应 row attribution 指向的真实 user 消息，不得匹配 assistant/tool 来源行。状态 kind 与锚点类型 SHALL 一致。

候选回放 transcript 中的状态顺序 SHALL 与原日志顺序一致，且所有状态 SHALL 恰好在其合法锚点插入一次。系统 SHALL NOT 为通过协议校验而移动、重锚定、重排或改写旧状态，也不得把 synthetic user 状态插入 assistant tool_calls 与尚未齐全的结果之间。状态日志降级 SHALL 仅清理 cache-only 日志并记录 WARN，不修改 canonical 历史，不因清理失败而使 turn 失败。

#### Scenario: Redis 命中逐 byte 重放
- **WHEN** 后续 turn 恢复到一个锚点全部有效的状态日志
- **THEN** 每条状态以相同内容插入相同 canonical 位置
- **AND THEN** 新用户 turn 的状态与原文继续追加在重放 transcript 之后

#### Scenario: Redis miss 开始新缓存纪元
- **WHEN** 状态 key 不存在、过期或读取失败
- **THEN** 系统从 MySQL canonical 历史继续对话并生成新的当前状态
- **AND THEN** 不把旧缓存缺失当作请求失败，也不要求供应商缓存仍然存在

#### Scenario: 任一锚点失效则整份日志作废
- **WHEN** 状态日志中至少一个 `before_client_msg_id` 或 `after_tool_call_id` 在 durable 重建中不存在
- **THEN** 系统不重放该日志中的任何状态，best-effort 清除失效日志并生成新状态
- **AND THEN** 不把状态插到相近但错误的消息位置

#### Scenario: Session 删除清理状态
- **WHEN** 一个 session 被删除
- **THEN** 既有消息缓存和 runtime 状态 key 都被清理，MySQL 中不存在需要额外清理的状态行

#### Scenario: 存在但位于工具组中间的锚点无效
- **WHEN** canonical 单元为 `assistant[A,B] → tool[A] → tool[B]`，旧 budget 锚定 A
- **THEN** 整份状态日志被拒绝，不插入该 budget 或日志中的其他状态
- **AND THEN** 不把 budget 自动移到 B 后面，真实 canonical 单元保持不变

#### Scenario: 未完成工具组不能接收状态
- **WHEN** assistant 声明 A、B，但 canonical 结果只有 A，状态锚点 A 能被找到
- **THEN** A 不被视为完整组尾，整份状态日志作废
- **AND THEN** replay 不向缺失 B 结果的组内插入 synthetic user 消息

#### Scenario: before-user 锚点必须命中真实用户
- **WHEN** `before_client_msg_id` 只在 assistant/tool 的来源行或非唯一的候选用户位置上命中
- **THEN** 整份状态日志作废，不部分插入其他可匹配状态

#### Scenario: 状态种类与锚点类型不匹配
- **WHEN** `tool_budget` 使用 before-user 锚点或 `current_status` 使用 after-tool 锚点
- **THEN** 整份状态日志作废，不依靠错误锚点类型猜测其位置

#### Scenario: 完整并行组尾支持原位回放
- **WHEN** canonical 包含按原调用序排列的 A、B、C 及全部 tool results，budget 锚定最终结果 C
- **THEN** budget 只在 C 的结果之后原样插入一次
- **AND THEN** assistant 的工具数组、三个结果及状态之前的历史前缀都不被修改

#### Scenario: 回放后的序号顺序同样需要有效
- **WHEN** 日志条目的 Seq 递增，但其锚点在 canonical 中的排列会导致回放状态逆序
- **THEN** 系统拒绝整份状态日志，不按 Seq 重新排列 canonical 消息或状态位置

#### Scenario: 序号存在间隔仍从最大值续接
- **WHEN** 日志 Seq 为 10、20、30 且全部语义锚点与回放顺序有效
- **THEN** 三条状态保留原 Seq 和内容，新状态从 31 继续，而不是从状态条数 3 推导

#### Scenario: 非法日志清理失败不阻断真实 turn
- **WHEN** 非法状态日志需要作废，但 Redis 清理操作失败
- **THEN** 本次请求仍不使用该日志，并从未修改的 canonical 历史开启新 epoch
- **AND THEN** 系统记录 WARN，用户原文、真实用户 ordinal 和 turn 执行不受缓存清理失败影响

### Requirement: Cache-only status is excluded from durable and user-facing records
运行时状态 SHALL NOT 写入 MySQL `messages`、`tool_calls`、`subagent_runs` 或其他会话 transcript，不得产生 SSE 事件、前端历史项、真实用户消息 ordinal 或标题输入。状态缺失时不得影响真实会话恢复。既有 `llm_raw_log` SHALL 继续捕获实际发送的完整请求，包括状态消息。

send-time 用户终稿 SHALL 只保存用户提交的正文及既有附件元数据，不再拼接 `prompt.RenderStatus()` 或等价内容。已有包含旧式内联 `<current_status>` 的 MySQL 记录 SHALL 按原字节读取，不做迁移。

#### Scenario: 新用户行只包含原文
- **WHEN** 用户发送正文 `hello`，模型请求同时包含独立时间状态
- **THEN** send-time MySQL 用户行与现有 `msgs:{sid}` 对应行只包含 `hello`
- **AND THEN** 标题生成和真实用户消息计数不观察 synthetic 状态

#### Scenario: Turn 完成后没有状态会话记录
- **WHEN** 一个包含多次倒计时的 turn 完成
- **THEN** MySQL/Redis durable 消息、SSE 和前端历史不包含 synthetic 状态
- **AND THEN** 原始 LLM 请求捕获仍保留实际发送的状态内容

#### Scenario: 旧内联状态保持兼容
- **WHEN** 读取本变更前已把时间状态拼入正文的用户行
- **THEN** 系统按既有规则重建该行，不执行回填、清洗或格式迁移

### Requirement: Tool-round countdown uses existing max_rounds semantics
Confucius 与每个通用 SubAgent SHALL 在工具派发后的后续模型请求前追加独立 `<tool_budget>`，至少包含 `rounds_used`、`max_rounds`、`rounds_remaining`。`rounds_used` SHALL 是当前 agent 已进入的 1 基工具循环轮次，`rounds_remaining` SHALL 为 `max(max_rounds-rounds_used, 0)`。同一 assistant 响应中的多个并行 tool calls SHALL 只消耗一轮；瞬时重试和 length continuation 尝试 SHALL NOT 增加轮次。

倒计时 SHALL 只提供提前收尾信息，不改变 round-cap 判定或新增单个工具调用硬上限。root 状态 SHALL 追加到 Redis 状态日志；SubAgent 状态 SHALL 只在单次 Run 的 active transcript 中追加，不写 root 状态日志。不同 agent 实例的计数 SHALL 独立。

#### Scenario: 并行调用只消耗一轮
- **WHEN** 某 agent 第 1 轮并行执行 3 个工具
- **THEN** 新状态报告 `rounds_used=1`、`rounds_remaining=max_rounds-1`
- **AND THEN** 不报告已用 3 轮，也不为每个工具结果插入一条用户消息

#### Scenario: 重试不追加状态
- **WHEN** 一个已携带最新状态的请求发生可重试的瞬时错误
- **THEN** 系统复用完全相同的已构造请求和 Redis 状态条目
- **AND THEN** 不生成新的 `status_id`，不推进 `rounds_used`

#### Scenario: Continuation 保持轮次但可记录新工具组
- **WHEN** length continuation 在同一外层轮次中执行了一个后续工具组
- **THEN** 可追加具有新工具组序号、相同 `rounds_used` 的最新状态
- **AND THEN** 旧状态不变，上一完整请求仍是下一请求前缀

#### Scenario: 零剩余进入现有 wrap-up
- **WHEN** 某 agent 已完成第 `max_rounds` 个工具轮次
- **THEN** 收尾请求携带 `rounds_remaining=0` 与既有 wrap-up 指令
- **AND THEN** tools 仍被省略，现有 `round_capped`、WARN 和错误处理保持不变

#### Scenario: SubAgent 计数与快照隔离
- **WHEN** 两个并行 SubAgent 具有不同已执行轮次并完成运行
- **THEN** 各自 active transcript 只包含自身倒计时
- **AND THEN** `subagent_runs.messages_json` 不包含任何 synthetic 状态

### Requirement: Current status reports conservative context availability
root 的 `<current_status>` SHALL 包含本 turn 固定 RFC3339 时间戳和 `context_usage`。上下文块至少报告 `estimated: true`、`estimated_input_tokens`、`input_budget_tokens`、`estimated_remaining_tokens`；输入预算 SHALL 使用当前 turn 的 `NewRequestBudget(max_context_tokens, max_completion_tokens)`，剩余量 SHALL clamp 到零。

完整请求估算 SHALL 覆盖 system prompt、tools、durable history、Redis 重放的全部旧状态、新 current_status、真实当前用户消息及当次协议脚手架。状态数值自身 SHALL 通过保守测量消息计入估算，最终报告余量不得大于最终候选请求的非负预算差值。provider 累计 `total_tokens` SHALL NOT 被当作当前上下文大小。

无有效正输入预算时，系统 SHALL 省略上下文数值而保留时间戳，不得编造零预算或无限预算。

#### Scenario: 首轮估算包含重放状态和新状态
- **WHEN** 一个 Redis 命中的 turn 已重放多份历史状态，并准备追加新的 current_status
- **THEN** 估算包含全部旧状态、新状态、真实用户输入、system/tools 和协议开销
- **AND THEN** 状态明确标记为估算并报告非负剩余量

#### Scenario: 状态自身不会绕过预算
- **WHEN** 加入状态后候选请求达到预算阈值
- **THEN** preflight 使用包含这些状态的较大估算执行既有 compaction 或超预算处理

#### Scenario: 超预算剩余量为零
- **WHEN** 最终候选请求估算达到或超过输入预算
- **THEN** `estimated_remaining_tokens` 为 `0`，既有错误/压缩行为继续执行

#### Scenario: 预算不可用时不伪造数值
- **WHEN** 手工构造的 agent 没有有效正输入预算
- **THEN** current_status 仍可携带时间戳，但不携带虚构的上下文余量

### Requirement: Cache misses and compaction explicitly start a new cache epoch
Redis miss、状态锚点校验失败和 compaction 实际改写 canonical projection SHALL 被视为可再生的新缓存纪元，而不是 durable 数据丢失。永久性的 `ContextCompacted` 标记本身 SHALL NOT 清理状态或开启新纪元；相同 checkpoint 的 stitched projection SHALL 保留 row attribution 并正常重放状态。compaction 输入与 checkpoint SHALL NOT 包含 synthetic 状态；成功压缩后系统 SHALL 清除该 session 的旧状态日志，并在新的 active transcript 中追加最新状态。压缩候选预算 SHALL 只计入压缩后仍会存在的新状态开销，且该开销 SHALL 在 retained-tail 选择前纳入 `RequestOverheadTokens`，不得在候选验收后无预算追加。

`terminalResponseFormat`、live overlay、工具配对、SubAgent snapshots 和持久化重建 SHALL 使用明确的 durable/canonical 投影或类型标记排除状态，不得依赖状态文本的偶然匹配。

#### Scenario: Compaction 清理旧状态并生成新状态
- **WHEN** mid-turn 或 preflight compaction 成功改写上下文
- **THEN** 旧 runtime 状态 key 被清理，压缩输入和 checkpoint 不含状态
- **AND THEN** 新 active transcript 追加基于压缩后完整候选请求计算的 current_status

#### Scenario: 相同 checkpoint 不开启新缓存纪元
- **WHEN** 一个已压缩 session 在连续两个 turn 使用同一 compaction record，没有发生新的 compaction
- **THEN** 第二个 turn 仍按锚点重放第一个 turn 的 current_status，上一完整请求保持为下一请求前缀
- **AND THEN** 永久 `ContextCompacted` 标记本身不清理 Redis 状态日志

#### Scenario: 压缩后按真实日志末尾续接序号
- **WHEN** 压缩后的 current_status 仅在内存中为 Seq 1，随后 Redis 只保存 Seq 2 的 tool_budget
- **THEN** 下一 turn 重放该 tool_budget 时携带其真实 Seq，并生成 Seq 3 的新状态
- **AND THEN** 第三个 turn 的重放校验继续通过，不因 `[2, 2]` 或按条数重新计数而作废整份日志

#### Scenario: 候选规划预留新状态开销
- **WHEN** 一个不含 current_status 开销的候选恰好达到输入预算，随后把保守 current_status 开销计入 `RequestOverheadTokens`
- **THEN** retained-tail planner 在发布候选前进一步缩减尾部
- **AND THEN** 最终包含实际新状态的请求仍不超过该预算

#### Scenario: Structured output 不受尾部倒计时影响
- **WHEN** 配置 `output_schema` 的 SubAgent 在工具结果及 tool_budget 后构造终轮请求
- **THEN** `terminalResponseFormat` 仍根据最后一个真实 tool result 判断终轮
- **AND THEN** `response_format` 不因 synthetic 状态而意外关闭

#### Scenario: 当前用户 live overlay 不识别状态
- **WHEN** 当前真实用户消息带图片且其前存在 current_status
- **THEN** `collectLiveMessages`、附件内联和 durable/live 匹配仍选择真实用户消息
