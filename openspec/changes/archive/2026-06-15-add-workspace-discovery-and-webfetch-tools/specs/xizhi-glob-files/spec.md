## ADDED Requirements

### Requirement: Xizhi glob files tool
Xizhi SHALL 提供基于 `doublestar` glob 模式在用户工作空间内递归搜索文件和目录的工具。

#### Scenario: Search files recursively with doublestar
- **WHEN** Agent 调用 `xizhi_glob_files`，pattern 为 `"src/**/*.go"`
- **THEN** 系统返回 `data/{user_uuid}/workspace/src` 下所有匹配 `.go` 文件的相对路径列表

#### Scenario: Search from subdirectory
- **WHEN** Agent 调用 `xizhi_glob_files`，path 为 `"internal"`，pattern 为 `"**/*_test.go"`
- **THEN** 系统返回 `data/{user_uuid}/workspace/internal` 下所有测试文件的相对路径列表

#### Scenario: Glob pattern matches directories
- **WHEN** Agent 调用 `xizhi_glob_files`，pattern 为 `"cmd/*"`
- **THEN** 系统返回 `data/{user_uuid}/workspace/cmd` 下所有直接子目录（以及匹配的文件）的相对路径列表

#### Scenario: No matches
- **WHEN** Agent 调用 `xizhi_glob_files`，pattern 不存在匹配项
- **THEN** 系统返回空列表

#### Scenario: Path outside workspace blocked
- **WHEN** Agent 调用 `xizhi_glob_files`，path 解析后超出 workspace
- **THEN** 系统拒绝操作，返回错误 "path outside workspace"
