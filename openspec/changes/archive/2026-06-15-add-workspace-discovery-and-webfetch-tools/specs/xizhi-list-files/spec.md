## ADDED Requirements

### Requirement: Xizhi list files tool
Xizhi SHALL 提供平铺列出用户工作空间指定目录下文件和子目录的工具。

#### Scenario: List files in workspace root
- **WHEN** Agent 调用 `xizhi_list_files`，path 为空或 `"."`
- **THEN** 系统返回用户工作空间根目录下的文件和子目录列表，包含每个条目的名称、类型（file/dir），以及文件大小

#### Scenario: List files in subdirectory
- **WHEN** Agent 调用 `xizhi_list_files`，path 为 `"src"`
- **THEN** 系统返回 `data/{user_uuid}/workspace/src` 目录下的文件和子目录列表

#### Scenario: Hidden files excluded by default
- **WHEN** Agent 调用 `xizhi_list_files`，未显式要求显示隐藏文件
- **THEN** 系统过滤掉名称以 `.` 开头的文件和目录

#### Scenario: Path outside workspace blocked
- **WHEN** Agent 调用 `xizhi_list_files`，path 为 `"../../etc"`
- **THEN** 系统拒绝操作，返回错误 "path outside workspace"

#### Scenario: List non-existent directory
- **WHEN** Agent 调用 `xizhi_list_files`，目标目录不存在
- **THEN** 系统返回错误 "directory not found"
