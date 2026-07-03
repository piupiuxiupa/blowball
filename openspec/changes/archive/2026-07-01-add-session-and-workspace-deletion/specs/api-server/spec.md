## ADDED Requirements

### Requirement: Deletion routes
系统 SHALL 在鉴权路由组内注册会话删除与工作空间删除两条路由。

#### Scenario: Session delete route
- **WHEN** 服务启动
- **THEN** 注册需要鉴权的 `DELETE /api/v1/sessions/:session_id`

#### Scenario: Workspace delete route
- **WHEN** 服务启动
- **THEN** 注册需要鉴权的 `DELETE /api/v1/workspace/files/*path`

#### Scenario: Deletion routes require auth
- **WHEN** 未携带有效 token 访问任一删除路由
- **THEN** 返回 HTTP 401
