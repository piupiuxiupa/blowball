# Delta Spec: sandbox-directory-configuration

## MODIFIED Requirements

### Requirement: Landlock 进程级目录可配置

系统 SHALL 支持顶层 `landlock` 配置块，字段包括 `enabled`（布尔，默认 `true`）、`system_read_only`（系统只读基线目录列表）、`extra_read_write`（额外进程可写目录）、`extra_read_only`（额外进程只读目录）。进程的 RW 应用目录默认为 `{data-dir}/data`、`{data-dir}/logs`、`{data-dir}/skills`，RO 应用目录默认为 `{data-dir}/tools` 与 `{data-dir}/skills-market`（技能市场目录，operator 维护、进程只读）——这两组应用目录由 `-d` 派生，本配置不改变其派生。`landlock.system_read_only` 省略时默认为 `["/etc", "/usr", "/bin", "/lib", "/lib64", "/proc"]`，`extra_*` 省略时默认为空。

#### Scenario: 默认配置复现既有行为

- **WHEN** 配置中省略整个 `landlock` 块
- **THEN** landlock 以 RW 目录 `{data-dir}/data`、`{data-dir}/logs`、`{data-dir}/skills`、RO 目录 `{data-dir}/tools`、`{data-dir}/skills-market`、系统只读基线 `["/etc","/usr","/bin","/lib","/lib64","/proc"]` 生效
- **AND** 相对引入 `skills-market` 之前的行为，仅新增对 `{data-dir}/skills-market` 的只读授权，其余文件访问范围不变

#### Scenario: 额外可写目录被授予进程

- **WHEN** `landlock.extra_read_write` 配置为 `["/var/cache/blowball"]` 且该目录存在
- **THEN** 进程在 Landlock 应用后可读写 `/var/cache/blowball`

#### Scenario: 显式禁用 Landlock

- **WHEN** `landlock.enabled` 为 `false`
- **THEN** 系统跳过 `ApplyLandlock` 调用并记录告警
- **AND** 不依赖内核级限制，仅保留应用层路径校验

## ADDED Requirements

### Requirement: Runtime skills-market directory creation

系统 SHALL 在启动时（`setupRuntime`，与 `tools` 目录同一序列）对 `{data-dir}/skills-market` 执行 `MkdirAll`——目录内容缺失/为空是无害的（空目录不产生任何挂载或清单条目）。目录内容由 operator 直接放盘维护（多机部署各自同步挂载，同 `tools` 目录约定）；该目录位于 `-d` 之下而不在 `data` 子树内，SHALL NOT 参与 `workspace-shared-storage` 的共享存储健康检查与 FUSE 锚点。

#### Scenario: 启动时创建市场目录

- **WHEN** 进程以任一角色启动且 `{data-dir}/skills-market` 不存在
- **THEN** 系统创建该目录，启动正常继续

#### Scenario: 空目录零行为

- **WHEN** `{data-dir}/skills-market` 为空目录
- **THEN** 不产生任何市场技能条目或挂载，系统行为与功能关闭时一致

#### Scenario: 不参与共享存储健康检查

- **WHEN** `storage.workspace.backend` 为 `shared`
- **THEN** 共享存储健康检查只针对 `{data-dir}/data`，`skills-market` 目录不参与检查
