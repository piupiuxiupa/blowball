## ADDED Requirements

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
