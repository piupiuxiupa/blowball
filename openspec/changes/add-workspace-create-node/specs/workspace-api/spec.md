## ADDED Requirements

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
