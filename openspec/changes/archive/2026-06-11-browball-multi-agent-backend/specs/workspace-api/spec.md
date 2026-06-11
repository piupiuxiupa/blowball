## ADDED Requirements

### Requirement: List workspace files
系统 SHALL 提供接口列出用户工作空间中的文件和目录。

#### Scenario: List files in workspace root
- **WHEN** 用户发送 GET /api/v1/workspace/files
- **THEN** 系统返回 data/{user_uuid}/workspace/ 下的文件和目录列表，每项包含 name、type (file/dir)、size、update_time

#### Scenario: List files in subdirectory
- **WHEN** 用户发送 GET /api/v1/workspace/files?path=src
- **THEN** 系统返回 data/{user_uuid}/workspace/src/ 下的文件和目录列表

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
系统 SHALL 提供接口返回当前可用的 MCP 工具列表。

#### Scenario: List MCP tools
- **WHEN** 用户发送 GET /api/v1/mcp/tools
- **THEN** 系统返回所有已注册的 Xizhi 工具定义（name、description、parameters schema）
