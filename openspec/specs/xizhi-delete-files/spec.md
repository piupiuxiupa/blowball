# xizhi-delete-files Specification

## Purpose

定义 `xizhi_delete` 工作空间文件删除工具的注册、行为、路径校验与工具描述，作为清理工作空间草稿/临时文件（如 `tmp/` 目录）的删除原语。

## Requirements

### Requirement: xizhi_delete tool registration
系统 SHALL 提供一个名为 `xizhi_delete` 的 Xizhi 工作空间文件工具，受 `tools.xizhi.delete.enabled` 开关控制；启用时注册到工具注册表，可被配置了该工具的 Agent 使用。其注册模式 SHALL 与 `xizhi_list_files`/`xizhi_tree`/`xizhi_glob_files` 一致（按 `tools.xizhi.<tool>.enabled` 条件注册）。

#### Scenario: Enable delete tool
- **WHEN** 配置中 `tools.xizhi.delete.enabled` 为 true
- **THEN** 系统将 `xizhi_delete` 注册到工具注册表，可被配置了该工具的 Agent 使用

#### Scenario: Disable delete tool
- **WHEN** 配置中 `tools.xizhi.delete.enabled` 为 false 或未配置
- **THEN** 系统不注册 `xizhi_delete`，Agent 无法调用它

### Requirement: Xizhi delete file or directory
`xizhi_delete` SHALL 在调用方用户的工作空间（`data/{userID}/workspace/`）内删除由相对路径 `path` 指向的文件或目录。当 `path` 指向目录时，SHALL 递归删除整个目录树（含子目录与文件）。删除不存在的路径 SHALL 视为成功（幂等）而非错误。所有路径解析与安全校验 SHALL 复用 `xizhi_*` 既有的 `validatePath`。

#### Scenario: Delete a file
- **WHEN** Chongzhi 调用 `xizhi_delete`，path 为 "tmp/scratch.txt"
- **THEN** 系统删除 `data/{userID}/workspace/tmp/scratch.txt`，返回成功结果

#### Scenario: Delete a directory recursively
- **WHEN** Chongzhi 调用 `xizhi_delete`，path 为 "tmp/scratch-dir"（含子文件与子目录）
- **THEN** 系统递归删除整个 `tmp/scratch-dir` 目录树，返回成功结果

#### Scenario: Delete non-existent path is idempotent
- **WHEN** Chongzhi 调用 `xizhi_delete`，path 指向不存在的路径
- **THEN** 系统返回成功（幂等），不返回错误

### Requirement: xizhi_delete path validation and reserved-directory rejection
`xizhi_delete` SHALL 与其它 `xizhi_*` 工具执行相同的应用层路径校验：拒绝绝对路径、经 `filepath.Clean` 后逃逸出工作空间的 `..`、以及经 `filepath.EvalSymlinks` 解析后落在工作空间之外的符号链接。`xizhi_delete` SHALL 拒绝任何其首个 cleaned 段为保留应用命名空间目录（`.blowball`）的路径，使工作空间内的应用状态（含 `.blowball/skills/`）只能经其专用工具（`luban_*`）管理，永不经文件删除工具触及。校验失败 SHALL 返回与其它 `xizhi_*` 工具一致的"含相对路径示例"的错误风格。

#### Scenario: Path traversal blocked
- **WHEN** Chongzhi 调用 `xizhi_delete`，path 为 "../../etc/passwd"
- **THEN** 系统拒绝操作，返回 "path outside workspace" 类错误并附相对路径示例

#### Scenario: Symlink escape blocked
- **WHEN** 工作空间内存在符号链接指向外部目录，且 `xizhi_delete` 的 path 经该符号链接
- **THEN** 系统使用 `filepath.EvalSymlinks` 解析真实路径后验证前缀，拒绝越界操作

#### Scenario: Delete under reserved directory blocked
- **WHEN** Agent 调用 `xizhi_delete`，path 为 ".blowball/skills/foo"
- **THEN** 系统拒绝操作并返回路径错误，引导模型用 `luban_*` 工具管理 skills

### Requirement: xizhi_delete description declares result shape
`xizhi_delete` 的工具描述 SHALL 声明其结果结构与关键语义：返回 `{path, deleted, type}`（`type` 为 `file`/`directory`/`none`，`none` 表示目标本不存在）；目录递归删除；不存在的路径幂等成功；**`path` MUST 为相对工作空间根的相对路径**（绝对路径、`..`、符号链接逃逸被拒绝）。描述 SHALL 指明其为清理工作空间草稿/临时文件的删除原语，供模型在提示词约定的 `tmp/` 清理中使用。

#### Scenario: delete 描述声明结果结构与相对路径
- **WHEN** `xizhi_delete` 工具被注册并渲染给模型
- **THEN** 描述声明返回 `{path, deleted, type}`，并声明 `path` 必须相对工作空间根

#### Scenario: delete 描述声明递归删除与幂等语义
- **WHEN** `xizhi_delete` 工具被注册并渲染给模型
- **THEN** 描述声明目录递归删除、不存在的路径幂等成功

#### Scenario: delete 描述指明为清理原语
- **WHEN** `xizhi_delete` 工具被注册并渲染给模型
- **THEN** 描述指明其为清理草稿/临时文件的删除原语，供 `tmp/` 清理使用
