## MODIFIED Requirements

### Requirement: Delete session

系统 SHALL 提供已鉴权的会话删除端点 `DELETE /api/v1/sessions/:session_id`，仅允许会话所有者删除。删除 SHALL 按以下次序执行：①先同步 drain 消息入库队列（使该会话尚未入库的消息先落 MySQL，保证归档完整，见 message-write-behind 的同步 drain 原语）→ ②在单个 MySQL 事务中将数据归档到 `*_deleted` 镜像表并删除源数据 → ③删除 Redis 中的 `session:{id}` 与 `msgs:{id}` 缓存 key（先库后缓存）。文件系统会话文件不再存在，无文件清理步骤。

#### Scenario: 所有者成功删除

- **WHEN** 已鉴权用户对属于自己的会话发送 `DELETE /api/v1/sessions/:session_id`
- **THEN** 系统先同步 drain 入库队列，然后在单个事务中把 sessions/titles/messages 原样写入对应 `*_deleted` 表，删除 `sessions` 行（级联清除 live 的 titles/messages），随后删除 Redis 中的 `session:{id}` / `msgs:{id}`，并返回 HTTP 204

#### Scenario: 归档包含未入库消息

- **WHEN** 删除时刻该会话仍有停留在入库队列（buffer 或 processing）中、尚未写入 MySQL 的消息
- **THEN** drain 步骤先将这些消息插入 MySQL，归档镜像表中 SHALL 包含它们，不因异步入库时序而遗漏

#### Scenario: 非所有者或会话不存在

- **WHEN** 用户删除不属于自己的会话，或不存在的会话
- **THEN** 系统返回 HTTP 404，且不进行任何归档或删除

#### Scenario: 未鉴权

- **WHEN** 请求未携带有效 token
- **THEN** 返回 HTTP 401

#### Scenario: Redis 缓存主动清理

- **WHEN** 会话被成功删除
- **THEN** 系统主动删除 Redis 中的 `session:{id}` / `msgs:{id}`（先完成数据库删除，再清缓存）；之后对该会话的读取因源会话已不存在而返回 404
