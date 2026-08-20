# turn-cost-tracking 变更规格

## MODIFIED Requirements

### Requirement: Per-turn token usage persistence
系统 SHALL 为每个完成的 chat turn 持久化一条 `turn_usage` 记录，记录该 turn 的总 token 用量、per-agent 明细与该 turn 实际使用的模型名（`model` 列），使历史成本可查、可按会话与按模型汇总、并为成本护栏提供数据基础。

#### Scenario: Successful turn persists usage
- **WHEN** 一次用户请求处理完成（成功路径），orchestrator 发射 done 事件
- **THEN** 系统向 `turn_usage` 表插入一行，包含 `session_id`、`trace_id`、`user_id`、`model`（该 turn 解析后的模型名）、`usage_json`（完整 usage 对象）与冗余的 `total_tokens`
- **AND THEN** `usage_json` 同时包含 `total`（聚合）与 `by_agent`（per-agent 拆分，键为 agent 名）两段

#### Scenario: Parallel sub-agent turn attributes per-agent cost
- **WHEN** 一个 turn 中 Confucius 并行调度了 Chongzhi 与 Liang
- **THEN** `turn_usage.usage_json.by_agent` 包含三个键：`Confucius`、`Chongzhi`、`Liang`，各自的 prompt_tokens/completion_tokens/total_tokens 反映该 agent 本 turn 的实际消耗
- **AND THEN** `by_agent` 各值之和等于 `total`

#### Scenario: Turn with no sub-agents
- **WHEN** 一个 turn 中 Confucius 未调度任何子 agent（直接回答）
- **THEN** `turn_usage.usage_json.by_agent` 仅含 `Confucius` 一个键，其值等于 `total`

#### Scenario: Model recorded for per-model cost aggregation
- **WHEN** 一个 turn 以请求参数选择了非缺省模型并完成
- **THEN** 该 turn 的 `turn_usage.model` 记录该模型名，存量行（本变更前）的 `model` 为 NULL
