# tool-call-log-correlation Specification

## Purpose

Correlate logs across a turn's handler, orchestration, agent, model, and tool-call path using the context-carried `session_id` and `trace_id`.

## Requirements

### Requirement: Context 感知的日志关联字段

系统 SHALL 提供 `logger.FromContext(ctx)`，返回附加了 ctx 所携带的 `session_id` 与 `trace_id` 字段的 logger；任一字段缺失时 SHALL 优雅省略该字段（不输出空值）。turn 链路（handler → orchestrator → agent → tool）上新输出与被改写的日志 SHALL 经由 `FromContext` 输出，使日志可按 `session_id` 与 `trace_id` 直接过滤定位。

#### Scenario: 携带双 id 的请求

- **WHEN** ctx 同时携带 `session_id` 与 `trace_id`（turn 入口 `MessageStreamHandler` 已注入）
- **THEN** `FromContext(ctx)` 输出的每条日志同时包含这两个字段

#### Scenario: 仅携带 trace_id 的请求（api 路径或后台）

- **WHEN** ctx 只有 `trace_id` 没有 `session_id`
- **THEN** 日志包含 `trace_id`，省略 `session_id` 字段，不输出空串

#### Scenario: 无任何 id 的 ctx

- **WHEN** ctx 既无 `session_id` 也无 `trace_id`
- **THEN** `FromContext` 返回的 logger 行为与全局 `L()` 一致，不新增字段

### Requirement: session-id context key 的共享归宿

`session_id` 的 context key 与读写函数 SHALL 定义于共享叶子包（`internal/pkg/reqctx`），使 `internal/pkg/logger` 与 `internal/tool` 无需依赖 `internal/agent` 即可读取。`internal/agent` 既有的 `WithSessionID`/`SessionIDFromContext` SHALL 保留为委托别名，公开 API 与既有注入点（`MessageStreamHandler`、`TitleService`、raw capture attribution）SHALL NOT 变更行为。

#### Scenario: 依赖方向

- **WHEN** `internal/tool` 或 `internal/pkg/logger` 需要读取 ctx 中的 `session_id`
- **THEN** 通过共享包完成，不产生对 `internal/agent` 的 import（无环）

#### Scenario: 注入点兼容

- **WHEN** turn 入口以 `agent.WithSessionID`（别名）注入 session id 后，工具层经共享包读取
- **THEN** 读到的值与注入值一致（同一 key）

### Requirement: Registry.Call 中央工具派发日志

系统 SHALL 在 `Registry.Call`（注册表工具的统一派发点）为每次调用输出一条结构化日志：成功时 INFO 级、失败时 WARN 级，字段包含 `event=tool_call`、`tool`（工具名）、`duration`（含超时包装的完整执行时长）、`args_preview`（截断的参数预览）；失败时 SHALL 额外携带 `error` 字段。日志 SHALL 经 `FromContext` 输出（携带 `session_id`/`trace_id`）。该日志 SHALL NOT 记录工具结果体，SHALL NOT 影响派发行为（日志失败不可能阻断工具调用）。

#### Scenario: 成功调用的 INFO 日志

- **WHEN** 任一 agent 经 `Registry.Call` 成功执行一个注册表工具（如 `xizhi_read_file`）
- **THEN** 输出一条 INFO 日志，含 `event=tool_call`、`tool`、`duration`、截断的 `args_preview`、ctx 携带的 `session_id` 与 `trace_id`

#### Scenario: 失败调用的 WARN 日志

- **WHEN** 注册表工具执行返回 error（含超时截断）
- **THEN** 输出一条 WARN 日志，在成功字段集之上附 `error` 字段

#### Scenario: 覆盖全部注册表工具

- **WHEN** 三个 agent 的任一配置工具或 per-turn 注册的 `mcp_*` 家族被调用
- **THEN** 均经过 `Registry.Call`、均产生派发日志（一处实现全覆盖）

### Requirement: 存量 turn 链路日志补齐关联字段

既有 turn 链路日志 —— agent 层 WARN（unexpected finish_reason、length continuation、round cap、orchestrator 错误）、`openai_client` 的 LLM request/response 与 raw-capture/idle-timeout 告警、executor audit 与危险命令告警、redis `logCmd`、handler 侧 turn 生命周期与 compaction 告警 —— SHALL 改为经 `FromContext` 输出，补齐 `session_id` 并保留既有 `trace_id`（已有者）。无会话上下文的后台任务日志（flusher、优雅关闭等）不在本要求范围内。

#### Scenario: executor audit 补齐双 id

- **WHEN** bash 工具执行并输出审计日志
- **THEN** 该日志在既有字段（command、user_id、exit_code 等）之上携带 `session_id` 与 `trace_id`

#### Scenario: round-cap 告警可定位

- **WHEN** agent 触发 round cap 并输出 WARN
- **THEN** 该 WARN 携带 `session_id` 与 `trace_id`，可与该 turn 的其他日志串联
