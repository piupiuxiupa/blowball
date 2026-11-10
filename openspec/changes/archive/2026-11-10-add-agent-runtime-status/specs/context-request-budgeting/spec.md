## ADDED Requirements

### Requirement: Cache-only runtime status is included in request-budget decisions
每次带有 `agent-runtime-status` 的模型请求，其 preflight 和上下文余量估算 SHALL 包含 active transcript 中全部已重放状态、已追加状态以及即将新增状态的 token 与协议开销。不得只估算 durable canonical 后忽略状态自身。新状态数值 SHALL 通过保守测量消息计入，最终报告的剩余量不得大于最终完整候选请求的非负预算差值。

compaction 成功后会清除旧状态，因此压缩候选 SHALL 只计入压缩后仍会存在的新状态开销，不得为将被丢弃的历史状态保留空间，也不得遗漏新状态。压缩完成后 SHALL 基于新 projection 和新状态重新估算。

`<current_status>` 中报告的 `input_budget_tokens` SHALL 与本次 preflight/compaction 使用的有效输入预算一致，`estimated_remaining_tokens` SHALL 是根据完整候选请求得到的保守非负余量，而不是模型窗口减累计 turn usage。

#### Scenario: 状态不会绕过 preflight 预算
- **WHEN** durable canonical 单独估算未超预算，但加入 Redis 重放状态和即将新增的状态后达到预算阈值
- **THEN** 系统按包含全部状态的估算执行既有 compaction 或超预算处理

#### Scenario: 压缩后只计算新状态
- **WHEN** preflight 或 mid-turn compaction 改写 canonical projection 并清理旧状态日志
- **THEN** 压缩候选不计入将被删除的旧状态，但计入压缩后追加的新 current_status
- **AND THEN** 发送前余量基于压缩后的完整候选请求重新计算

#### Scenario: Redis miss 后按新纪元估算
- **WHEN** 状态日志 miss 或锚点失效，系统从 MySQL canonical 历史生成新状态
- **THEN** 估算不包含无法重放的旧状态，只包含干净历史与本次新状态

#### Scenario: 上下文余量不使用累计 turn usage
- **WHEN** 一个 turn 已累计多轮 prompt/completion tokens
- **THEN** 当前上下文状态仍按当次完整请求估算与输入预算计算
- **AND THEN** 不把跨轮累计 `total_tokens` 误当作当前上下文大小
