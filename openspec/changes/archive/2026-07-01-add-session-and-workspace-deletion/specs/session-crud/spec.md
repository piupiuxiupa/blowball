## ADDED Requirements

### Requirement: Delete session
系统 SHALL 提供已鉴权的会话删除端点 `DELETE /api/v1/sessions/:session_id`，仅允许会话所有者删除；删除时先将数据归档到 `*_deleted` 镜像表，再清除源数据并清理文件系统会话文件；Redis 缓存不做主动处理（依赖 TTL 自然过期）。

#### Scenario: 所有者成功删除
- **WHEN** 已鉴权用户对属于自己的会话发送 `DELETE /api/v1/sessions/:session_id`
- **THEN** 系统在单个事务中把 sessions/titles/messages 原样写入对应 `*_deleted` 表，删除 `sessions` 行（级联清除 live 的 titles/messages），删除 FS 会话 JSON 文件 `data/{userID}/sessions/{session_id}.json`，并返回 HTTP 204

#### Scenario: 非所有者或会话不存在
- **WHEN** 用户删除不属于自己的会话，或不存在的会话
- **THEN** 系统返回 HTTP 404，且不进行任何归档或删除

#### Scenario: 未鉴权
- **WHEN** 请求未携带有效 token
- **THEN** 返回 HTTP 401

#### Scenario: Redis 不主动清理
- **WHEN** 会话被成功删除
- **THEN** 系统不主动删除 Redis 中的 `session:{id}` / `msgs:{id}`，依赖 TTL 自然过期；之后对该会话的读取因源会话已不存在而返回 404
