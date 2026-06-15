## ADDED Requirements

### Requirement: Xizhi tree tool
Xizhi SHALL 提供以树形结构递归展示用户工作空间指定目录的工具，支持深度限制。

#### Scenario: Render tree with default depth
- **WHEN** Agent 调用 `xizhi_tree`，path 为空或 `"."`，未指定 depth
- **THEN** 系统返回从工作空间根目录开始、深度为 3 的目录树结构

#### Scenario: Render tree with custom depth
- **WHEN** Agent 调用 `xizhi_tree`，path 为 `"src"`，depth 为 2
- **THEN** 系统返回 `data/{user_uuid}/workspace/src` 下深度为 2 的目录树结构

#### Scenario: Depth exceeds maximum
- **WHEN** Agent 调用 `xizhi_tree`，depth 大于 10
- **THEN** 系统将实际遍历深度限制为 10 并返回结果

#### Scenario: Hidden entries excluded by default
- **WHEN** Agent 调用 `xizhi_tree`
- **THEN** 系统默认不返回名称以 `.` 开头的文件和目录

#### Scenario: Path outside workspace blocked
- **WHEN** Agent 调用 `xizhi_tree`，path 解析后超出 workspace
- **THEN** 系统拒绝操作，返回错误 "path outside workspace"
