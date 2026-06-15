## MODIFIED Requirements

### Requirement: Xizhi tool registry
系统 SHALL 提供工具注册表，根据 Agent 配置中的 tools 列表动态构建 OpenAI function calling 的 tools 参数。

#### Scenario: Build tools for agent
- **WHEN** Agent 需要调用 OpenAI API
- **THEN** 系统根据 Agent 配置的 tools 列表，从注册表中查找对应的 tool definition，构造 tools 参数；注册表中除包含 read/write/modify 外，还包含 list_files、tree、glob_files 等 workspace-scoped 工具

#### Scenario: Tool not found in registry
- **WHEN** Agent 配置引用了不存在的 tool name
- **THEN** 服务启动时报错并拒绝启动

## ADDED Requirements

### Requirement: Xizhi tool configuration
系统 SHALL 在 `tools.xizhi` 配置下为每个 Xizhi 工具提供独立的 `enabled` 开关。

#### Scenario: Enable list files tool
- **WHEN** 配置中 `tools.xizhi.list_files.enabled` 为 true
- **THEN** 系统将 `xizhi_list_files` 注册到工具注册表，可被 Agent 使用

#### Scenario: Enable tree tool
- **WHEN** 配置中 `tools.xizhi.tree.enabled` 为 true
- **THEN** 系统将 `xizhi_tree` 注册到工具注册表，可被 Agent 使用

#### Scenario: Enable glob files tool
- **WHEN** 配置中 `tools.xizhi.glob_files.enabled` 为 true
- **THEN** 系统将 `xizhi_glob_files` 注册到工具注册表，可被 Agent 使用
