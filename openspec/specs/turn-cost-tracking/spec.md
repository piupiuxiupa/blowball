# turn-cost-tracking Specification

## Purpose

定义每个 chat turn 的 token 成本持久化与归因能力：为每个完成的 turn 记录一条 `turn_usage` 行，承载总 token 用量与 per-agent 明细，使历史成本可查、可按会话汇总，并为成本护栏（如重试预算）提供数据基础。

## Requirements

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

### Requirement: Usage written independently of message persistence
`turn_usage` 的写入 SHALL 作为独立调用执行，SHALL NOT 与消息持久化共事务（消息批次自 Redis-first 改造后不再存在同步 MySQL 事务；即便消息仍处于入库队列中，usage 行也可独立先行落地）。usage 写入失败 SHALL NOT 影响消息持久化（usage 是观测数据，消息是业务数据，优先级不同）。

#### Scenario: Usage written as an independent call
- **WHEN** 一轮 turn 的消息批次完成 Redis 双写
- **THEN** `turn_usage` 行通过独立的 `SaveTurnUsage` 调用写入 MySQL，其时序与消息 flusher 的异步入库互不依赖（usage 行可能先于消息行落地）

#### Scenario: Usage write failure does not roll back messages
- **WHEN** `turn_usage` 插入失败（如临时 DB 错误）
- **THEN** 系统记录错误日志但不影响消息批次的队列写入与后续 flush
- **AND THEN** SSE 响应与消息持久化不受影响（usage 是观测数据，消息是业务数据，优先级不同）

### Requirement: Turn usage cascades with session deletion
`turn_usage` 行 SHALL 通过外键 `session_id → sessions.session_id ON DELETE CASCADE` 随会话删除而清理。

#### Scenario: Session deletion removes usage rows
- **WHEN** 一个会话被 `SessionService.DeleteSession` 删除
- **THEN** 该 session_id 下所有 `turn_usage` 行由数据库级联删除，无需应用层显式清理

### Requirement: Usage JSON shape is authoritative
`turn_usage.usage_json` 与 done 事件 `Meta.usage` SHALL 持有相同的对象形状，作为 per-agent 成本归因的权威定义。形状为：`{total: {prompt_tokens, completion_tokens, total_tokens, reasoning_tokens?}, by_agent: {<agent>: {...}}, meta: {sub_agent_invocations: [...], parallel: bool}}`。

#### Scenario: Shape matches done event exactly
- **WHEN** 系统构造 turn 的 usage 对象
- **THEN** 落库的 `usage_json` 与 done 事件 `Meta.usage` 是同一个对象的不同序列化（内容一致）

#### Scenario: Reasoning tokens included when present
- **WHEN** 某 agent 产生了 reasoning tokens（`thinking: true`）
- **THEN** 该 agent 在 `by_agent` 下的对象包含 `reasoning_tokens` 字段，`total` 也包含聚合的 `reasoning_tokens`

#### Scenario: Parallelism metadata recorded
- **WHEN** 一个 turn 中存在某个 assistant 轮次同时调度了 ≥2 个 tool_call
- **THEN** `usage_json.meta.parallel` 为 `true`
- **AND THEN** `usage_json.meta.sub_agent_invocations` 列出该 turn 派发的所有 invoke_* 工具名（按调用顺序）

### Requirement: Turn usage row keyed by trace
`turn_usage` SHALL 以 `(session_id, trace_id)` 标识单次请求的成本，并索引 `(session_id, created_at)` 以支持按会话时间序的成本查询。

#### Scenario: Per-request granularity
- **WHEN** 同一会话连续发起两次请求
- **THEN** 系统插入两条 `turn_usage` 行，各自携带不同的 `trace_id`

#### Scenario: Session cost aggregation queryable
- **WHEN** 运营查询某会话的累计成本
- **THEN** 可通过 `SELECT SUM(total_tokens) FROM turn_usage WHERE session_id=?` 直接得到，无需解析 `usage_json`
