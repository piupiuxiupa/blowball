# workspace-api Specification

## Purpose

定义工作空间文件管理与元数据查询能力，包括文件列表、上传、下载、文本内容读取、用户数据目录结构、skills 列表以及 MCP 工具列表接口。

## Requirements

### Requirement: List workspace files
系统 SHALL 提供接口列出用户工作空间中的文件和目录，并支持通过 `include_hidden` 参数控制是否返回隐藏文件和目录。

#### Scenario: List files in workspace root (hidden excluded by default)
- **WHEN** 用户发送 GET /api/v1/workspace/files
- **THEN** 系统返回 data/{user_uuid}/workspace/ 下的非隐藏文件和目录列表，隐藏条目（名称以 "." 开头）默认不出现

#### Scenario: List files including hidden entries
- **WHEN** 用户发送 GET /api/v1/workspace/files?include_hidden=true
- **THEN** 系统返回 data/{user_uuid}/workspace/ 下的全部文件和目录列表，包括隐藏条目

#### Scenario: List files in subdirectory (hidden excluded by default)
- **WHEN** 用户发送 GET /api/v1/workspace/files?path=src
- **THEN** 系统返回 data/{user_uuid}/workspace/src/ 下的非隐藏文件和目录列表

#### Scenario: List files in subdirectory including hidden entries
- **WHEN** 用户发送 GET /api/v1/workspace/files?path=src&include_hidden=true
- **THEN** 系统返回 data/{user_uuid}/workspace/src/ 下的全部文件和目录列表，包括隐藏条目

#### Scenario: List empty workspace
- **WHEN** 用户工作空间为空
- **THEN** 系统返回 HTTP 200，body 为空数组 []

### Requirement: Upload file
系统 SHALL 提供文件上传接口，将文件保存到用户工作空间。

#### Scenario: Upload file successfully
- **WHEN** 用户发送 POST /api/v1/workspace/upload，multipart form 包含文件和 path 参数
- **THEN** 系统将文件保存到 data/{user_uuid}/workspace/{path}/，返回文件路径和大小

#### Scenario: Upload to path outside workspace
- **WHEN** 上传路径解析后不在 workspace 内
- **THEN** 系统返回 HTTP 403，拒绝操作

#### Scenario: Upload file too large
- **WHEN** 上传文件超过配置的最大文件大小限制
- **THEN** 系统返回 HTTP 413，错误信息 "file too large"

### Requirement: Download file
系统 SHALL 提供文件下载接口。

#### Scenario: Download existing file
- **WHEN** 用户发送 GET /api/v1/workspace/files/:path
- **THEN** 系统返回文件内容，Content-Type 根据文件扩展名设置

#### Scenario: Download non-existent file
- **WHEN** 请求的文件不存在
- **THEN** 系统返回 HTTP 404

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

### Requirement: Get file content as text
系统 SHALL 提供接口以 JSON 格式返回文件文本内容。

#### Scenario: Get text file content
- **WHEN** 用户发送 GET /api/v1/workspace/files/:path/content
- **THEN** 系统返回 HTTP 200，body 为 {"path": "...", "content": "文件内容", "size": 1234}

#### Scenario: Get binary file content
- **WHEN** 请求的文件为二进制文件（图片、压缩包等）
- **THEN** 系统返回 HTTP 400，提示 "binary file, use download endpoint"

### Requirement: User data directory structure
每个用户的文件 SHALL 按固定结构组织在 data/{user_uuid}/ 下。

#### Scenario: Auto create user directories
- **WHEN** 新用户首次登录或首次操作
- **THEN** 系统创建 data/{user_uuid}/ 目录及子目录 sessions/、workspace/、skills/

### Requirement: Skills list
系统 SHALL 提供接口返回用户 skills 列表。

#### Scenario: List skills
- **WHEN** 用户发送 GET /api/v1/skills
- **THEN** 系统扫描 data/{user_uuid}/skills/ 目录，返回文件列表作为可用 skills

#### Scenario: No skills
- **WHEN** 用户 skills 目录为空
- **THEN** 系统返回 HTTP 200，body 为空数组 []

### Requirement: MCP tool list
系统 SHALL 提供接口返回当前可用的 MCP 工具列表，列表 SHALL 包含所有已注册的内置工具以及通过 MCP client 注册的外部 MCP 代理工具。

#### Scenario: List MCP tools
- **WHEN** 用户发送 GET /api/v1/mcp/tools
- **THEN** 系统返回所有已注册工具的定义（name、description、parameters schema），包括 Xizhi 工具、webfetch、invoke_chongzhi / invoke_liang 以及外部 MCP 代理工具

#### Scenario: External MCP tools appear in list
- **WHEN** 配置中声明了一个外部 MCP server 且该 server 返回至少一个工具
- **THEN** GET /api/v1/mcp/tools 的响应中包含该外部工具的定义

#### Scenario: Disabled external MCP server contributes no tools
- **WHEN** 某个外部 MCP server 初始化失败或被禁用
- **THEN** GET /api/v1/mcp/tools 的响应中不包含该 server 的工具，且系统不因该 server 失败而影响其他工具返回

### Requirement: Delete workspace file or directory
系统 SHALL 提供已鉴权的工作空间删除端点 `DELETE /api/v1/workspace/files/*path`，支持递归删除文件或目录；路径校验与现有读取/下载接口一致（复用 `xizhi.ValidatePath`，越界返回 403）。文件无数据库源表，删除时不写任何归档。

#### Scenario: 删除文件
- **WHEN** 已鉴权用户对工作空间内的文件发送 `DELETE /api/v1/workspace/files/<path>`
- **THEN** 系统删除该文件，返回 HTTP 204

#### Scenario: 递归删除目录
- **WHEN** 已鉴权用户对工作空间内的目录发送 `DELETE /api/v1/workspace/files/<dir>`
- **THEN** 系统递归删除该目录及其全部内容，返回 HTTP 204

#### Scenario: 路径越界被拒绝
- **WHEN** 路径解析后超出 workspace（绝对路径、含 `..`、或符号链接逃逸）
- **THEN** 返回 HTTP 403

#### Scenario: 目标不存在
- **WHEN** 目标文件或目录不存在
- **THEN** 返回 HTTP 404

#### Scenario: 不写数据库归档
- **WHEN** 工作空间文件或目录被删除
- **THEN** 系统不在 MySQL 写入任何归档记录（文件无源表），仅从文件系统移除
