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

系统 SHALL 提供接口返回用户 skills 列表。当 `skill_market.url` 启用时（见 `skill-market` 能力），响应 SHALL 合并市场 allowlist 条目：市场条目的 `name`/`description` 取自市场 API、`location` 为 `"skill_market"`；名字冲突时本地条目胜出。市场拉取 SHALL 使用该 HTTP 请求自身的 `Authorization` 头（token 与 login 返回一致），不需要 turn context 管道；拉取失败时 SHALL 降级为仅本地列表（fail-closed，WARN），接口仍返回 HTTP 200。

#### Scenario: List skills
- **WHEN** 用户发送 GET /api/v1/skills
- **THEN** 系统扫描 data/{user_uuid}/skills/ 目录，返回文件列表作为可用 skills

#### Scenario: No skills
- **WHEN** 用户 skills 目录为空，且市场功能关闭或 allowlist 为空
- **THEN** 系统返回 HTTP 200，body 为空数组 []

#### Scenario: Market entries merged and labeled
- **WHEN** `skill_market.url` 已启用，市场服务对该用户的 JWT 返回含 `fund-promo-sentiment-v2` 的清单
- **THEN** 响应包含该条目，`location` 为 `"skill_market"`，`description` 为市场 API 返回值

#### Scenario: Local entry wins on name conflict
- **WHEN** 用户目录存在技能 `foo`，市场 allowlist 亦含名为 `foo` 的技能
- **THEN** 响应中 `foo` 只出现本地版本，市场条目被覆盖

#### Scenario: Market failure degrades to local-only
- **WHEN** 市场服务不可达或返回非 200
- **THEN** 接口返回仅含本地技能的列表，HTTP 200，系统记 WARN

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

### Requirement: Search workspace entries

系统 SHALL 提供 `GET /api/v1/workspace/search` 端点,在认证用户的工作区内按条目名递归搜索文件与目录(JWT 鉴权;`api` 角色路由分区,`api` 与 `all` 角色注册,`agent` 角色不注册)。

**参数**(全部 query 参数):`pattern`(可选;**子串语义**,服务端转义为字面量后匹配条目名 basename;空/纯空白等价于匹配一切)、`type`(可选;枚举 `file`/`dir`/`any`,缺省 `any`)、`path`(可选;搜索根,相对工作区根,缺省为工作区根;空/纯空白等价于根;SHALL 经 `xizhi.ValidatePathAllowReserved` 校验——绝对路径、`..`、符号链接逃逸拒绝,`.blowball` 保留命名空间**允许**,与 List 需求同一校验原语)、`max_depth`(可选;非负整数,搜索根之下的组件数,缺省不限)、`ignore_case`(可选布尔,缺省 **true**)、`include_hidden`(可选布尔,缺省 false)、`head_limit`/`offset`(可选整数,缺省 200/0;`applied_limit`/`applied_offset` 回显实际生效值)。

**匹配与遍历语义** SHALL 与 `xizhi_find` 引擎一致:pattern 仅匹配条目名(basename)不匹配路径;不跟随符号链接;`.gitignore` 等忽略文件不参与;隐藏条目(名以 `.` 开头)默认排除、`include_hidden: true` 时包含;收集 SHALL 设上限(实现固定,约 10000 条),超限停止遍历并置 `truncated: true`。entries SHALL 按路径字典序稳定排序(分页骨架;系统 SHALL NOT 做相关性排序)。实现 SHALL 复用 fd/纯 Go 双引擎(`fd`/`fdfind` 在 `PATH` 时 shell out,否则纯 Go walk),两引擎产出逐字段一致。

**响应** 200 SHALL 为 `{total, truncated, applied_limit, applied_offset, entries: [...]}`;每条 entry SHALL 携带 `path`(工作区相对路径,`/` 分隔)、`name`(basename)、`type`(`"file"` 或 `"dir"`,与 List 的类型值域一致)、`size`(字节)、`update_time`(RFC3339,UTC)。`size`/`update_time` SHALL 仅对返回页(≤ `applied_limit` 条)逐条 stat,SHALL NOT 对全量命中 stat。遍历与 stat 之间条目消失(TOCTOU)时系统 SHALL 保留该条目并以 `size: 0`、`update_time: ""` 降级,SHALL NOT 静默跳过(保持本页条数与分页窗口一致)。

**超时**:整个搜索(遍历 + stat)SHALL 受服务端总超时约束(固定实现常量,约 10 秒);超时 SHALL 返回 500 与错误码 `SEARCH_TIMEOUT`。

**错误映射**:搜索根越界 SHALL 返回 403 `FORBIDDEN`;搜索根不存在 SHALL 返回 200 与空 entries(`total: 0`);搜索根为普通文件 SHALL 返回 400;`type` 非法、`max_depth` 为负、`head_limit`/`offset` 非法整数 SHALL 返回 400;未携带有效凭据 SHALL 返回 401。

#### Scenario: 子串搜索命中文件与目录

- **WHEN** 工作区存在 `reports/2026/q1.md` 与 `reports/backup/`,已认证用户请求 `GET /workspace/search?pattern=2026`
- **THEN** 系统返回 200,entries 包含 `reports/2026`(type `dir`)下名字含 `2026` 的条目;`reports/2026/q1.md` 的 basename `q1.md` 不含 `2026`,不被返回(pattern 只匹配 basename)

#### Scenario: 空 pattern 配合类型过滤枚举

- **WHEN** 请求 `GET /workspace/search?pattern=&type=dir`
- **THEN** 系统返回工作区内所有非隐藏目录(等价于按类型全量枚举),entries 按路径字典序

#### Scenario: 忽略大小写默认开启

- **WHEN** 工作区存在 `README.md`,请求 `GET /workspace/search?pattern=readme`(不带 `ignore_case`)
- **THEN** 系统返回 `README.md`;显式 `ignore_case=false` 时不返回

#### Scenario: 特殊字符按字面量匹配

- **WHEN** 工作区存在 `report(1).md` 与 `a+b.txt`,请求 `GET /workspace/search?pattern=report(1)`
- **THEN** 系统返回 `report(1).md`,不因 `(` 触发正则语义或报错

#### Scenario: 隐藏条目默认排除,include_hidden 可见

- **WHEN** 工作区存在 `.blowball/skills/x/SKILL.md`,请求 `GET /workspace/search?pattern=SKILL`
- **THEN** 默认(`include_hidden` 缺省)不返回该条目;`include_hidden=true` 时返回

#### Scenario: .blowball 可作为搜索根

- **WHEN** 请求 `GET /workspace/search?path=.blowball/skills&type=file`
- **THEN** 校验通过(AllowReserved),在该子树内搜索;agent 侧 `xizhi_find` 对同一路径仍然拒绝(两路径校验原语不同,互不影响)

#### Scenario: 搜索根越界拒绝

- **WHEN** 请求 `GET /workspace/search?path=../escape` 或 `path=/etc`
- **THEN** 系统返回 403 `FORBIDDEN`

#### Scenario: 搜索根不存在返回空 200

- **WHEN** 请求 `GET /workspace/search?path=no/such/dir`
- **THEN** 系统返回 200,`total: 0`,entries 为空数组

#### Scenario: 搜索根为普通文件拒绝

- **WHEN** 请求 `GET /workspace/search?path=notes.txt`(该路径是文件)
- **THEN** 系统返回 400,错误信息说明搜索根必须是目录

#### Scenario: 分页与截断

- **WHEN** 某 pattern 命中 350 条,请求 `head_limit=200&offset=0`
- **THEN** 返回 200 条、`total: 350`、`truncated: true`、`applied_limit: 200`、`applied_offset: 0`;以 `offset=200` 翻页取得其余 150 条,两次结果无重叠无遗漏(字典序稳定排序保证)

#### Scenario: 条目在 stat 前消失降级

- **WHEN** 某命中条目在引擎遍历之后、handler stat 之前被删除
- **THEN** 该条目仍出现在返回页中,`size: 0`、`update_time: ""`,本页条数与 `applied_limit` 一致

#### Scenario: 超时返回显式错误

- **WHEN** 搜索(遍历或 stat)超过服务端超时常量
- **THEN** 系统返回 500 与错误码 `SEARCH_TIMEOUT`,不挂死连接

#### Scenario: 非法参数拒绝

- **WHEN** 请求 `type=folder`,或 `max_depth=-1`,或 `head_limit=abc`
- **THEN** 系统返回 400

#### Scenario: 缺少鉴权

- **WHEN** 未携带有效 Bearer token 请求 `GET /workspace/search`
- **THEN** 系统返回 401

#### Scenario: stat 携带大小与修改时间

- **WHEN** 命中文件 `reports/q1.md`(4096 字节,2026-08-01T09:00:00Z 修改)
- **THEN** entry 为 `{path: "reports/q1.md", name: "q1.md", type: "file", size: 4096, update_time: "2026-08-01T09:00:00Z"}`
