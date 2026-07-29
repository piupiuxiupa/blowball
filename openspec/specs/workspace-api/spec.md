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

### Requirement: Write file content as text
系统 SHALL 提供已鉴权的文本内容写入端点 `PUT /api/v1/workspace/files/*path/content`，作为现有 `GET .../content`（“Get file content as text”）读取接口的对称写入能力。路径 SHALL 经 `xizhi.ValidatePathAllowReserved` 校验（与读取/下载/删除等 REST 接口一致，越界返回 403；REST 接口允许 `.blowball/` 命名空间，区别于 agent 的 `xizhi_*` 工具）。

写入 SHALL 为“创建或替换”（HTTP PUT 语义，与 `xizhi_write_file` 一致）：目标文件不存在时创建（必要时 `MkdirAll` 创建父目录），已存在时整体替换。写入 SHALL 原子完成——先写入目标所在目录下的临时文件，写入成功后再用 `os.Rename` 替换目标；任何中途失败（磁盘满、写错误、超限）都不改动原文件。请求体 SHALL 受服务配置的 `maxUploadBytes` 上限约束。

该端点仅写文本：当 `content` 包含 NUL 字节时 SHALL 返回 400 `BINARY_FILE`（与读取侧拒绝返回二进制的策略对称，保证写入的内容可被 `GET .../content` 原样读回）；二进制与大文件仍走 `POST .../upload`。

#### Scenario: 创建新文本文件
- **WHEN** 已鉴权用户发送 `PUT /api/v1/workspace/files/notes.md/content`，body 为 `{"content": "hello"}`，且 `notes.md` 不存在
- **THEN** 系统创建该文件（必要时创建父目录），返回 HTTP 200，body 为 `{"path": "notes.md", "size": 5}`

#### Scenario: 原子覆盖已有文本文件
- **WHEN** 已鉴权用户对一个已存在的文本文件发送 `PUT /api/v1/workspace/files/notes.md/content` 写入新内容
- **THEN** 系统先写临时文件再 `os.Rename` 替换；返回 HTTP 200，body 为 `{"path": "notes.md", "size": <新字节数>}`；任何中途失败都不改动原文件

#### Scenario: 自动创建嵌套父目录
- **WHEN** 已鉴权用户发送 `PUT /api/v1/workspace/files/a/b/c.md/content`，且 `a/b/` 不存在
- **THEN** 系统创建 `a/b/` 后写入 `c.md`，返回 HTTP 200

#### Scenario: 文本含 NUL 字节被拒绝
- **WHEN** `content` 包含 NUL 字节（即试图写入二进制内容）
- **THEN** 系统返回 HTTP 400，body 包含 `BINARY_FILE`，原文件不变

#### Scenario: 超过最大尺寸被拒绝
- **WHEN** 请求体超过配置的 `maxUploadBytes`
- **THEN** 系统返回 HTTP 413，错误信息 `file too large`，原文件不变

#### Scenario: 路径越界被拒绝
- **WHEN** `path` 解析后超出 workspace（绝对路径、含 `..`、或符号链接逃逸）
- **THEN** 系统返回 HTTP 403，body 包含 `FORBIDDEN`

#### Scenario: 目标是已存在目录
- **WHEN** `path` 解析为一个已存在的目录
- **THEN** 系统返回 HTTP 400，body 包含 `BAD_REQUEST`

#### Scenario: 缺少鉴权
- **WHEN** 请求未携带有效 JWT
- **THEN** 系统返回 HTTP 401

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

### Requirement: Create empty file or directory
系统 SHALL 提供已鉴权的"创建节点"端点 `POST /api/v1/workspace/files/*path`,通过请求体 `{"type": "file" | "directory"}` 指定创建文件或目录。路径取自 URL 的 catch-all 参数(与 rename 一致:身份在 URL,参数在 body)。该端点 SHALL 受 `AuthMiddleware` 保护(缺少有效 JWT 返回 401),并在 `api`(及 `all`)角色中由 `wireAPI` 挂载。

路径 SHALL 经 `xizhi.ValidatePathAllowReserved` 校验(与读取/下载/写入/重命名/删除等 REST 接口一致,越界——绝对路径、含 `..`、或符号链接逃逸——返回 403;REST 接口允许 `.blowball/` 命名空间,区别于 agent 的 `xizhi_*` 工具)。当解析后的路径为空(即试图"创建"workspace 根本身)时 SHALL 返回 400 `BAD_REQUEST`。

创建 SHALL 为**严格创建**:目标叶子节点已存在时(无论它是文件还是目录)SHALL 返回 409 `ALREADY_EXISTS`,且不改动已有节点。文件创建 SHALL 通过 `os.OpenFile(abs, O_CREATE|O_EXCL|O_WRONLY, 0o644)` 完成,目录创建 SHALL 通过 `os.Mkdir(abs, 0o755)` 完成——二者均以 `EEXIST` 表示叶子已存在,从而把"不存在"与"创建"合并为单次原子操作,消除 check-then-create 的竞态窗口。

缺失的父目录 SHALL 被自动创建(`os.MkdirAll(filepath.Dir(abs), 0o755)`),即"叶子严格 + 自动建父":一条嵌套路径(如 `a/b/c`)在一次调用中即可建立,严格的"不可覆盖已有叶子"契约只针对叶子节点本身。

`type` 字段为必填,取值 SHALL 恰为 `"file"` 或 `"directory"`;缺失或为其它值时 SHALL 返回 400 `BAD_REQUEST`。成功时 SHALL 返回 HTTP 200,body 为 `{"path": <相对路径>, "type": <"file"|"directory">}`。

#### Scenario: 创建空文件
- **WHEN** 已鉴权用户发送 `POST /api/v1/workspace/files/notes.md`,body 为 `{"type": "file"}`,且 `notes.md` 不存在
- **THEN** 系统创建一个空文件(0 字节),返回 HTTP 200,body 为 `{"path": "notes.md", "type": "file"}`

#### Scenario: 创建空目录
- **WHEN** 已鉴权用户发送 `POST /api/v1/workspace/files/sub`,body 为 `{"type": "directory"}`,且 `sub` 不存在
- **THEN** 系统创建该目录,返回 HTTP 200,body 为 `{"path": "sub", "type": "directory"}`

#### Scenario: 自动创建嵌套父目录
- **WHEN** 已鉴权用户发送 `POST /api/v1/workspace/files/a/b/c`,body 为 `{"type": "directory"}`,且 `a/b/` 不存在
- **THEN** 系统先创建 `a/b/` 再创建 `c/`,返回 HTTP 200,body 为 `{"path": "a/b/c", "type": "directory"}`

#### Scenario: 目标文件已存在被拒绝
- **WHEN** 已鉴权用户对一个已存在的文件发送 `POST /api/v1/workspace/files/notes.md`,body 为 `{"type": "file"}`
- **THEN** 系统返回 HTTP 409,body 包含 `ALREADY_EXISTS`,原文件不变

#### Scenario: 目标目录已存在被拒绝
- **WHEN** 已鉴权用户对一个已存在的目录发送 `POST /api/v1/workspace/files/sub`,body 为 `{"type": "directory"}`
- **THEN** 系统返回 HTTP 409,body 包含 `ALREADY_EXISTS`,原目录不变(与"目标文件已存在"行为一致)

#### Scenario: 创建 workspace 根被拒绝
- **WHEN** 已鉴权用户发送 `POST /api/v1/workspace/files/`(路径为空)或 `*path` 解析为 workspace 根
- **THEN** 系统返回 HTTP 400,body 包含 `BAD_REQUEST`

#### Scenario: type 缺失或非法被拒绝
- **WHEN** 已鉴权用户发送 `POST /api/v1/workspace/files/notes.md`,body 不含 `type`,或 `type` 为 `"folder"` 等非 `{file, directory}` 的值
- **THEN** 系统返回 HTTP 400,body 包含 `BAD_REQUEST`

#### Scenario: 路径越界被拒绝
- **WHEN** `path` 解析后超出 workspace(绝对路径、含 `..`、或符号链接逃逸)
- **THEN** 系统返回 HTTP 403,body 包含 `FORBIDDEN`

#### Scenario: 缺少鉴权
- **WHEN** 请求未携带有效 JWT
- **THEN** 系统返回 HTTP 401
