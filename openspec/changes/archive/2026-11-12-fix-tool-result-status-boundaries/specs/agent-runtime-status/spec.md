## MODIFIED Requirements

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
