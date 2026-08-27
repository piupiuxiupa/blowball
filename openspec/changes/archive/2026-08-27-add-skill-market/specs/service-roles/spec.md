# Delta Spec: service-roles

## MODIFIED Requirements

### Requirement: Shared runtime data root

`api` 与 `agent` 角色 SHALL 从同一个 `-d`/`--data-dir` 派生 `data`、`logs`、`skills`、`tools`、`skills-market` 五类落盘位置，并连接同一份 MySQL、Redis 与共享 POSIX 文件系统存储；两角色之间不存在数据面隔离，三层持久化、xizhi 工作空间工具、executor 沙箱与 Landlock 策略的行为均与单进程时一致。该共享 POSIX 文件系统默认为本地磁盘；当配置为共享模式（见 `workspace-shared-storage`，实现为 MinIO 支撑的 JuiceFS 挂载）时，`data` 子树 SHALL 由 operator 在进程启动前挂载就绪，使跨机器的 `api`/`agent` 实例读写同一份 per-user 数据；`skills-market` 子树不参与该共享模式检查，由 operator 在每台主机各自同步维护（同 `tools` 约定）。当 `skill_market.url` 启用时，`api` 角色 SHALL 为 skills 列表端点构造市场客户端（仅 HTTP 客户端装配，不引入 orchestrator/OpenAI 依赖），`agent` 角色 SHALL 为 luban 工具与 executor 沙箱构造同一客户端。

#### Scenario: 两角色共享同一数据目录

- **WHEN** `api` 与 `agent` 角色以相同的 `-d /var/lib/blowball` 启动
- **THEN** 两者读写相同的 `/var/lib/blowball/data`、`/var/lib/blowball/logs`、`/var/lib/blowball/skills`、`/var/lib/blowball/tools`、`/var/lib/blowball/skills-market`

#### Scenario: 两角色共享同一存储后端

- **WHEN** 两角色同时运行
- **THEN** 两者连接配置中同一 MySQL 与 Redis；agent 角色写入的 turn 结果可被 api 角色读取

#### Scenario: 跨机器实例经共享文件系统共享数据面

- **WHEN** `storage.workspace.backend` 为 `shared`，且分别位于两台机器的 `api` 与 `agent` 实例将 JuiceFS 挂载到各自的 `{data-dir}/data`（指向同一 MinIO bucket + 同一元数据引擎）
- **THEN** 一台机器上对 `{data-dir}/data/{userID}/` 的写对另一台机器可见，工作空间与 per-user 数据跨机共享，不存在数据面隔离

#### Scenario: api 角色独立提供含市场条目的 skills 列表

- **WHEN** `skill_market.url` 已启用，仅 `api` 角色运行，用户请求 `GET /api/v1/skills`
- **THEN** api 角色以该请求的 JWT 拉取市场清单并返回合并结果，不依赖 `agent` 角色进程
