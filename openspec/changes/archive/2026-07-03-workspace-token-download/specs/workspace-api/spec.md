## ADDED Requirements

### Requirement: Download file via URL token
系统 SHALL 提供通过 URL query 参数传递 JWT 的文件下载接口。

#### Scenario: Download existing file with token
- **WHEN** 用户发送 `GET /api/v1/workspace/files/download?token=<valid_jwt>&path=reports/2026-q2.md`
- **THEN** 系统校验 token，返回文件内容，HTTP 200，Content-Disposition 为 `attachment; filename="2026-q2.md"; filename*=utf-8''2026-q2.md`

#### Scenario: Inline preview with token
- **WHEN** 用户发送 `GET /api/v1/workspace/files/download?token=<valid_jwt>&path=reports/2026-q2.md&inline=1`
- **THEN** 系统返回文件内容，HTTP 200，Content-Disposition 为 `inline; filename="2026-q2.md"; filename*=utf-8''2026-q2.md`

#### Scenario: Chinese filename encoding
- **WHEN** 用户请求下载路径为 `reports/中文报告.md`
- **THEN** Content-Disposition 包含 `filename*=utf-8''%E4%B8%AD%E6%96%87%E6%8A%A5%E5%91%8A.md`

#### Scenario: Missing token
- **WHEN** 用户发送请求未提供 `token` 参数
- **THEN** 系统返回 HTTP 401，body 包含 `missing token`

#### Scenario: Invalid or expired token
- **WHEN** 用户提供的 token 签名错误、格式非法或已过期
- **THEN** 系统返回 HTTP 401，body 包含 `invalid token` 或 `token expired`

#### Scenario: Path outside workspace
- **WHEN** 用户请求 `path=../../../etc/passwd`
- **THEN** 系统返回 HTTP 403，body 包含 `FORBIDDEN`

#### Scenario: Path is a directory
- **WHEN** 用户请求的路径解析为目录
- **THEN** 系统返回 HTTP 400，body 包含 `BAD_REQUEST`

#### Scenario: File not found
- **WHEN** 用户请求的文件不存在
- **THEN** 系统返回 HTTP 404，body 包含 `NOT_FOUND`
