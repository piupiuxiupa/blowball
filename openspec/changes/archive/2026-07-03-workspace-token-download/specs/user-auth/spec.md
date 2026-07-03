## ADDED Requirements

### Requirement: Authenticate via URL token
系统 SHALL 允许特定公开端点通过 URL query 参数 `token` 完成 JWT 鉴权。

#### Scenario: Valid token in query parameter
- **WHEN** 请求 `GET /api/v1/workspace/files/download?token=<valid_jwt>&path=reports/2026-q2.md`
- **THEN** `QueryTokenAuthMiddleware` 校验 token，将 `user_id` 注入 gin.Context，请求继续处理

#### Scenario: Missing query token
- **WHEN** 请求未包含 `token` 参数
- **THEN** 中间件返回 HTTP 401，body 包含 `missing token`

#### Scenario: Invalid query token
- **WHEN** 请求包含的 `token` 签名错误或格式非法
- **THEN** 中间件返回 HTTP 401，body 包含 `invalid token`

#### Scenario: Expired query token
- **WHEN** 请求包含的 `token` 已过期
- **THEN** 中间件返回 HTTP 401，body 包含 `token expired`

#### Scenario: Header Authorization is ignored on token-only endpoint
- **WHEN** 请求同时携带 `Authorization: Bearer <valid_jwt>` 和错误的 `?token=bad`
- **THEN** 中间件只校验 `?token=`，返回 HTTP 401
