## MODIFIED Requirements

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
