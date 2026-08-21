## ADDED Requirements

### Requirement: Session detail

系统 SHALL 提供已鉴权的单会话详情端点 `GET /api/v1/sessions/:session_id`，仅会话所有者可读。响应 SHALL 为会话列表条目的超集：`session_id`、`title`（无标题时空字符串）、`create_time`、`update_time`（RFC3339 UTC），以及与列表一致的活跃 turn 标记 —— 存在运行中 turn 时 `generating: true` 且携带 `run_id`（活动 run 认领判据，同 "Session list" 需求），否则 `generating: false` 且不携带 `run_id`。不存在的会话与不属于当前用户的会话 SHALL 返回 404，且不泄露他人会话的存在性。`context_compacted` 等内部标记 SHALL NOT 出现在响应中。

#### Scenario: Retrieve session detail

- **WHEN** 用户发送 GET /api/v1/sessions/:session_id，会话存在且属于当前用户
- **THEN** 系统返回 HTTP 200，body 包含 `session_id`、`title`、`create_time`、`update_time`、`generating` 字段

#### Scenario: Generating flag mirrors active run claim

- **WHEN** 该会话存在运行中 turn
- **THEN** 响应携带 `"generating": true` 与 `"run_id": "<当前运行中 turn 的 run id>"`
- **AND** 无运行中 turn 时 `generating` 为 false 且不携带 `run_id`

#### Scenario: Active-run read failure degrades

- **WHEN** 活跃 run 认领读取失败（如 Redis 不可用）
- **THEN** 端点仍返回 HTTP 200，`generating` 为 false，服务端记录 WARN 日志

#### Scenario: Unauthorized session access

- **WHEN** 用户请求不属于自己会话的详情
- **THEN** 系统返回 HTTP 404

#### Scenario: Missing session

- **WHEN** 用户请求不存在的 session_id
- **THEN** 系统返回 HTTP 404

#### Scenario: Title read failure degrades

- **WHEN** 会话行读取成功但标题读取失败
- **THEN** 端点仍返回 HTTP 200，`title` 为空字符串，服务端记录 WARN 日志
