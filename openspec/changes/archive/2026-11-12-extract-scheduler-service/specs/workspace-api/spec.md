## MODIFIED Requirements

### Requirement: Write file content as text

系统 SHALL 保留已鉴权 PUT /api/v1/workspace/files/*path/content，与文本读取对称。路径 SHALL 经现有 ValidatePathAllowReserved 安全检查；REST 的 .blowball 访问保持，但 SHALL 额外应用 .cron 受管理路径保护，只允许编辑授权 draft，发布区、staging、元数据及受保护祖先不可经该端点覆盖。

合法文本写入 SHALL 保留创建/替换、必要父目录创建、同目录临时文件和原子 rename；失败不改原文件。请求体 SHALL 受 maxUploadBytes 限制；NUL 文本返回 400 BINARY_FILE，二进制仍走上传，上传也遵循同样的 .cron 保护。

#### Scenario: 创建新文本文件
- **WHEN** 已鉴权用户向不存在的 notes.md 写 hello
- **THEN** 创建并返回 200 与 path/size

#### Scenario: 原子覆盖已有文本文件
- **WHEN** 合法已有文件收到文本替换
- **THEN** 先临时写再 rename，失败不改变原内容

#### Scenario: 自动创建嵌套父目录
- **WHEN** 合法 a/b/c.md 的父目录不存在
- **THEN** 创建父目录并返回 200

#### Scenario: 文本含 NUL 字节被拒绝
- **WHEN** content 包含 NUL
- **THEN** 返回 400 BINARY_FILE，原文件不变

#### Scenario: 超过最大尺寸被拒绝
- **WHEN** 请求体超 maxUploadBytes
- **THEN** 返回 413，原文件不变

#### Scenario: 路径越界被拒绝
- **WHEN** 路径为绝对路径、穿越或符号链接逃逸
- **THEN** 返回 403 FORBIDDEN，不越界读写

#### Scenario: 目标是已存在目录
- **WHEN** 文本目标是目录
- **THEN** 返回 400 BAD_REQUEST

#### Scenario: 缺少鉴权
- **WHEN** JWT 缺失或无效
- **THEN** 返回 401

#### Scenario: 修改任务草稿
- **WHEN** owner 修改自己合法 Task 的 .cron/<task_id>/draft 文件
- **THEN** 按受控 draft revision/写入围栏原子完成，当前发布版本不变

#### Scenario: 覆盖已发布任务资源
- **WHEN** 通用文本写入目标为 revisions、staging 或管理元数据
- **THEN** 返回 403 RESOURCE_READ_ONLY，不修改资源

## ADDED Requirements

### Requirement: Cron namespace protection across workspace mutations

工作空间列表/下载/文本读取 SHALL 在 owner 授权下支持任务草稿及发布资源，默认隐藏条目规则保持。上传、创建、删除、rename、Office 写回和文件工具 SHALL 共用 .cron 保护规则，包括路径祖先和移动目标；不能通过通用接口删除整个任务根来破坏发布资源。他人 Task/资源 SHALL 返回 404；受保护写入 SHALL 返回明确 403。

#### Scenario: List resources with hidden entries enabled
- **WHEN** owner 请求 include_hidden=true 并读取受管理 .cron
- **THEN** 可以查看授权资源，默认 workspace 根列表仍隐藏 .cron

#### Scenario: Rename bypass is attempted
- **WHEN** 请求通过覆盖/移动 Task 根或发布区祖先绕过只读保护
- **THEN** 操作拒绝，草稿与发布版本没有部分移动

#### Scenario: Another user's task identifier is supplied
- **WHEN** 用户在资源路径或发布接口使用他人 task_id
- **THEN** 返回 404，不访问该用户目录
