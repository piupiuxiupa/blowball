## ADDED Requirements

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
