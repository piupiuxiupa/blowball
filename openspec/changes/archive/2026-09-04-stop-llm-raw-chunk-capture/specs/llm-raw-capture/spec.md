## ADDED Requirements

### Requirement: 每次 LLM 调用的原始记录含请求与终态行

系统 SHALL 在每次 LLM 调用(所有经由 `LLMClient.StreamChat` 的调用方:Confucius、Chongzhi、Liang、round-cap 收尾轮、title 生成)产生 `kind=request` 行(调用发起时)以及 `kind=response` 或 `kind=error` 行(调用结束时;缝合版保留)。系统 MUST NOT 为新的 SSE chunk 产生 `kind=chunk` 行。同一调用的全部行 MUST 共享同一 `call_id`(uuid)与同一 `seq`(trace 内单调递增的调用序号),request 行 MUST 使用 `frame_index=0`,response/error 行 MUST 使用 `frame_index=1`,并携带 `trace_id`、`session_id`、`user_id`、`agent`、`model` 列。

#### Scenario: 成功的流式调用
- **WHEN** 一次 `StreamChat` 调用正常完成(网关发送 N 个 SSE chunk 并正常结束)
- **THEN** 落库 2 行:`kind=request`(frame_index=0)与 `kind=response`(frame_index=1),全部共享 `call_id` 与 `seq`,且无论 N 为多少都不产生新的 `kind=chunk` 行

#### Scenario: 调用中途进程崩溃
- **WHEN** `StreamChat` 发出请求后、流结束前进程崩溃
- **THEN** 缓冲中已存在该调用的 `kind=request` 记录;系统不因调用未完成而补造终态行,也不为崩溃前已到达的帧产生新的 chunk 行

#### Scenario: 调用失败(网关错误)
- **WHEN** 网关返回非 2xx(如 400/500)导致流失败
- **THEN** 落库 `kind=request` + `kind=error` 两行;error 行的 `http_status` 为网关状态码,`raw` 为网关返回的原始错误 body(不可得时为 error string)

#### Scenario: 收尾轮与 title 调用的归属
- **WHEN** round-cap 收尾轮执行,或 title 生成调用 LLM
- **THEN** 前者记录的 `agent` 为命中轮次上限的所属 agent;后者为 `"title"`;两者的 `session_id`/`trace_id` 与触发它们的 turn 一致

## MODIFIED Requirements

### Requirement: 原始 payload 保真入库

`kind=request` 行的 `raw` MUST 为实际发送的完整请求 JSON(messages、tools、全部参数),在 params 组装完成后捕获,而非重建近似。`kind=response` 行的 `raw` MUST 为由流式分片缝合重构的完整响应等价 JSON,保留聚合结构(内容、tool_calls、finish_reason、usage、reasoning 及网关扩展字段)。`raw` 列 SHALL 为 MEDIUMTEXT;request/response/error 行系统 MUST NOT 压缩或截断。系统 MUST NOT 为新的流式帧产生 `kind=chunk` 行;既有 `kind=chunk` 行 SHALL 继续按其写入时的原始帧 payload 读取。

#### Scenario: 请求侧为实发全量
- **WHEN** 一次带工具与思维参数的调用被捕获
- **THEN** `kind=request` 的 `raw` 可解析出与实发 `ChatCompletionNewParams` 一致的 messages 数组、tools 数组与参数字段

#### Scenario: 响应侧含被丢弃字段
- **WHEN** 网关在流中返回 usage 或扩展字段
- **THEN** `kind=response` 的 `raw` 含聚合后的该字段,不因现有 `LLMResponse` 结构体未承载而丢失

### Requirement: llm_raw_log 表结构与查询

系统 SHALL 提供 MySQL 表 `llm_raw_log`,列含 `id`(自增主键)、`call_id`、`seq`、`frame_index`、`trace_id`、`session_id`、`user_id`、`agent`、`kind`、`model`、`finish_reason`、`http_status`、`duration_ms`、`raw`(MEDIUMTEXT)、`msg_time`、`update_time`。表 MUST 建立 `(session_id, seq, frame_index)`、`(trace_id, seq)`、`(msg_time)` 索引。`model`、`finish_reason`、`http_status`、`duration_ms`、`msg_time`、`update_time` 为可从 raw 推导的冗余物化列,支持免解析直查。单条 INSERT 失败(如超出 MEDIUMTEXT 上限)SHALL 仅丢弃该条并记日志,MUST NOT 阻塞批次内其余记录落库。系统 SHALL 保留 `chunk` kind 与 `frame_index` 的历史含义以读取旧数据。

#### Scenario: 按会话回放调用序列
- **WHEN** 运维按 `session_id` 查询并 `ORDER BY seq, frame_index`
- **THEN** 得到该会话各 LLM 调用的有序请求/终态记录;旧数据中的历史 chunk 行仍可按同一顺序读取,无需解析 raw 即可经 `agent`/`model`/`kind`/`finish_reason`/`duration_ms` 列定位与筛选

#### Scenario: 单条超限隔离
- **WHEN** 批次中某条 raw 超过 MEDIUMTEXT 上限导致该行插入失败
- **THEN** 该条被丢弃并记 ERROR 日志,批次内其余行正常落库

### Requirement: Watchdog abort is captured as an error row

When the LLM stream idle watchdog aborts a captured call, the sink SHALL emit a `kind=error` row (not the partial `kind=response` row reserved for local context cancellation) with `http_status=0` and the typed error message as the raw body — carrying the configured idle, frames received, and model so the gap remains diagnosable. The row MUST retain the call's `call_id`/`seq`, use `frame_index=1`, and be the terminal row for the call. Local context cancellation SHALL continue to produce the partial `kind=response` row, unchanged.

#### Scenario: Idle-timeout abort produces an error row with diagnostics
- **WHEN** a captured streaming call is aborted by the idle watchdog
- **THEN** the call's terminal row is `kind=error` with `http_status=0` and a raw body naming the idle timeout plus frames received and model; the call emits no chunk rows

#### Scenario: Client disconnect remains a partial response row
- **WHEN** a captured streaming call ends because the caller's context was canceled
- **THEN** the call's final row is the existing stitched partial `kind=response` row, with no error row added

## REMOVED Requirements

### Requirement: 每次 LLM 调用的原始记录含逐帧 chunk 行

**Reason**: Per-frame rows dwarf the useful request/response evidence, making `llm_raw_log` unnecessarily large and hard to search.
**Migration**: New calls emit request plus response/error rows only. Existing `kind=chunk` rows remain queryable; operators may explicitly archive or delete them.

### Requirement: 逐帧捕获的累计截断

**Reason**: With new per-frame capture removed, the frame budget and truncation marker have no input to bound.
**Migration**: No code path emits new truncation markers. Historical marker rows remain readable as ordinary `kind=chunk` JSON payloads.
