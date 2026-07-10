## MODIFIED Requirements

### Requirement: Session list
系统 SHALL 返回当前用户的会话列表，包含 session_id 和标题。

#### Scenario: List sessions
- **WHEN** 用户发送 GET /api/v1/sessions
- **THEN** 系统返回 HTTP 200，body 为会话数组，每项包含 session_id 和 title，按 update_time 降序排列

#### Scenario: Session update_time reflects latest message activity
- **WHEN** 用户向一个已存在的会话发送新消息且该 turn 的消息成功持久化
- **THEN** 系统 SHALL 刷新该会话的 `sessions.update_time`
- **AND THEN** 该会话在后续 GET /api/v1/sessions 列表中出现在最前面
