# service-roles Delta

## ADDED Requirements

### Requirement: Standalone role-pinned service binaries

系统 SHALL 提供与单体等价的独立服务二进制:`blowball-api`(固定 api 角色)与 `blowball-agent`(固定 agent 角色)。二者 SHALL 接受与单体一致的 `-f/--config` 与 `-d/--data-dir` flag,使用同一配置 schema;SHALL NOT 提供 `--role` flag(传入 SHALL 非零退出)。各自的路由注册、监听端口与内部 RPC 端点 SHALL 与单体对应角色完全一致。`blowball-api` SHALL 附带 `seed` 子命令,行为与单体 seed 一致。

#### Scenario: blowball-api 独立启动
- **WHEN** 运行 blowball-api(合法配置)
- **THEN** 仅注册 API 路由分区并在 server.port 监听,未配置 openai.api_key 仅产生告警

#### Scenario: blowball-agent 独立启动
- **WHEN** 运行 blowball-agent(合法配置)
- **THEN** 仅注册 Agent 路由分区并在 server.agent_port 监听,提供 scheduledruntime 内部 RPC 服务端

#### Scenario: 独立二进制拒绝 --role
- **WHEN** 运行 blowball-api --role agent 或 blowball-agent --role api
- **THEN** 非零退出,不启动服务

#### Scenario: api 二进制的 seed 子命令
- **WHEN** 运行 blowball-api seed --username alice
- **THEN** 行为与单体 blowball seed 完全一致

#### Scenario: 与单体角色行为一致
- **WHEN** 以相同配置分别运行 blowball-api/blowball-agent 与单体 --role api/--role agent
- **THEN** 路由集合、监听端口、错误行为逐项一致

### Requirement: Thin assembly entry reuse

独立二进制与开发单体 SHALL 复用同一套服务装配包(`services/apiserver` 与 `services/agent` 的导出装配),SHALL NOT 各自维护 wiring 副本;角色差异 SHALL 只体现在装配入口选择,而非重复实现。

#### Scenario: 装配单一来源
- **WHEN** 检查 blowball-api、blowball-agent 与根 cmd/blowball 的服务构造代码
- **THEN** 三者均调用 services/apiserver 与 services/agent 的导出装配包,无重复 wiring
