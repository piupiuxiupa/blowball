## MODIFIED Requirements

### Requirement: Message list response format
The system SHALL return each message in the response using the canonical message data model, including all event types, so the client can reconstruct the full interaction history. Messages carrying image attachments SHALL include an `attachments` array of `{id, filename, mime}` references; messages without attachments SHALL omit the field.

#### Scenario: Full event stream returned
- **WHEN** a session contains user messages and assistant events
- **THEN** the response includes every stored message row with fields: id, session_id, msg_time, agent, msg_index, role, event_type, content, trace_id, update_time, and attachments (attachment-bearing rows only)

#### Scenario: Attachment metadata returned without join
- **WHEN** a user message row carries an `attachments_json` reference list
- **THEN** the list response embeds the references as the `attachments` array sourced from the message row itself, with no attachments-table join required

#### Scenario: Default ascending order
- **WHEN** no order parameter is provided
- **THEN** messages are ordered by (msg_time ASC, msg_index ASC)

#### Scenario: Descending order supported
- **WHEN** order=desc is provided
- **THEN** messages are ordered by (msg_time DESC, msg_index DESC)

### Requirement: Delete session

系统 SHALL 提供已鉴权的会话删除端点 `DELETE /api/v1/sessions/:session_id`，仅允许会话所有者删除。删除 SHALL 按以下次序执行：①先同步 drain 消息入库队列（使该会话尚未入库的消息先落 MySQL，保证归档完整，见 message-write-behind 的同步 drain 原语）→ ②在单个 MySQL 事务中将数据归档到 `*_deleted` 镜像表并删除源数据——该事务 SHALL 同步覆盖 `attachments` 行（写入 `attachments_deleted` 后删除 live 行）→ ③删除 Redis 中的 `session:{id}` 与 `msgs:{id}` 缓存 key（先库后缓存）→ ④移除该会话的附件目录 `data/{userID}/attachments/{session_id}`（幂等；失败仅记录日志，不使删除请求失败）。工作区文件不属于会话删除范围。

#### Scenario: 所有者成功删除

- **WHEN** 已鉴权用户对属于自己的会话发送 `DELETE /api/v1/sessions/:session_id`
- **THEN** 系统先同步 drain 入库队列，然后在单个事务中把 sessions/titles/messages 原样写入对应 `*_deleted` 表并归档/删除该会话全部 `attachments` 行，删除 `sessions` 行（级联清除 live 的 titles/messages），随后删除 Redis 中的 `session:{id}` / `msgs:{id}`，再移除附件目录，并返回 HTTP 204

#### Scenario: 归档包含未入库消息

- **WHEN** 删除时刻该会话仍有停留在入库队列（buffer 或 processing）中、尚未写入 MySQL 的消息
- **THEN** drain 步骤先将这些消息插入 MySQL，归档镜像表中 SHALL 包含它们，不因异步入库时序而遗漏

#### Scenario: 附件随会话归档清理

- **WHEN** 被删除的会话存在 3 条附件注册行与对应磁盘文件
- **THEN** 事务内 3 行写入 `attachments_deleted` 并从 `attachments` 删除，事务提交后 `attachments/{session_id}` 目录被移除；目录移除失败时删除仍成功并记录 ERROR 日志

#### Scenario: 无附件会话删除不变

- **WHEN** 被删除的会话没有任何附件
- **THEN** 附件归档/目录移除步骤为无操作，删除结果与既有行为一致

#### Scenario: 非所有者或会话不存在

- **WHEN** 用户删除不属于自己的会话，或不存在的会话
- **THEN** 系统返回 HTTP 404，且不进行任何归档或删除

#### Scenario: 未鉴权

- **WHEN** 请求未携带有效 token
- **THEN** 返回 HTTP 401

#### Scenario: Redis 缓存主动清理

- **WHEN** 会话被成功删除
- **THEN** 系统主动删除 Redis 中的 `session:{id}` / `msgs:{id}`（先完成数据库删除，再清缓存）；之后对该会话的读取因源会话已不存在而返回 404
