# sandbox-directory-configuration Specification

## Purpose

定义 Landlock（进程级）与 bwrap executor 沙箱（每命令级）目录控制的配置契约：可配的系统只读基线（两机制共享的概念）、landlock 的额外 RW/RO 目录、bwrap 的额外挂载，以及各自的默认值与校验守卫。默认值逐字段复现配置化之前的硬编码行为，确保零行为变更。承载性不变量（`/workspace`、`$HOME`、`$HOME/.local/bin`、skills 目标路径）保持固定不可配。

## Requirements

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

### Requirement: bwrap 沙箱目录映射可配置
系统 SHALL 支持位于 `tools.executor.sandbox` 的配置块，字段包括 `system_read_only`（沙箱系统只读基线，默认 `["/usr","/bin","/lib","/lib64","/etc"]`）、`extra_read_only`（operator 追加的只读挂载）、`extra_read_write`（operator 追加的可写挂载）。额外挂载条目为字符串，支持 `host`（沙箱内 target 同 host）或 `host:target`（自定义沙箱内路径）两种形式。

#### Scenario: 默认沙箱挂载复现既有行为
- **WHEN** 配置中省略整个 `tools.executor.sandbox` 块
- **THEN** bwrap 以系统基线 `["/usr","/bin","/lib","/lib64","/etc"]` 只读绑定、工作区挂到 `/workspace`、`$HOME` 合成为 `/home/blowball`、tools 挂到 `$HOME/.local/bin`
- **AND** 沙箱挂载表与配置化之前一致

#### Scenario: 额外只读数据集在沙箱内可读
- **WHEN** `tools.executor.sandbox.extra_read_only` 配置为 `["/opt/models"]`
- **THEN** 每次 executor 命令执行时 `/opt/models` 被只读绑定进沙箱（target 同 `/opt/models`）
- **AND** 沙箱内命令可读取 `/opt/models` 下的文件

#### Scenario: host:target 自定义挂载路径
- **WHEN** `extra_read_only` 配置为 `["/srv/datasets:/data"]`
- **THEN** 宿主 `/srv/datasets` 被只读绑定到沙箱内 `/data`

#### Scenario: 承载性不变量不可配置
- **WHEN** operator 尝试通过任何配置字段改变 `/workspace`、`/home/blowball`、`$HOME/.local/bin`、`/skills/global`、`/skills/user` 的沙箱内目标路径
- **THEN** 配置不提供改变这些路径的字段
- **AND** 这些路径在 `buildBwrapArgs` 中保持固定，PYTHONPATH（`/workspace/.pip`）、`--chdir /workspace`、PATH 前缀逻辑不受影响

### Requirement: 系统只读基线经 stat 守卫
landlock 与 bwrap 在应用各自 `system_read_only` 基线前，SHALL 对每个条目执行存在性检查（`os.Stat`），仅对实际存在的目录施加限制/绑定，缺失的条目被跳过并记录告警。该行为对两机制一致。

#### Scenario: 缺失系统基线目录不致启动失败
- **WHEN** 运行环境缺少 `/lib64`（如 aarch64）且 `system_read_only` 使用默认值
- **THEN** bwrap 起沙箱时跳过对 `/lib64` 的 `--ro-bind`，沙箱正常启动
- **AND** landlock 跳过对 `/lib64` 的限制并记录告警

### Requirement: 配置校验守卫
配置加载时 SHALL 强制下列守卫，违反则拒绝启动：`landlock.enabled` 为真时有效 RW 目录集合（默认应用 RW 目录 ∪ `extra_read_write`）非空；所有 landlock/bwrap 配置目录与额外挂载的 `host` 为绝对路径；`landlock.extra_read_write` 与 `tools.executor.sandbox.extra_read_write` 均不得包含 `"/"`；额外挂载的 `target` 不得与固定不变量（`/workspace`、`/home`、`/skills`、`/tmp`、`/proc`、`/dev`）或系统基线条目冲突。

#### Scenario: Landlock 缺少可写目录则拒绝启动
- **WHEN** `landlock.enabled` 为 `true` 且有效 RW 目录集合为空
- **THEN** 配置校验返回错误，系统拒绝启动

#### Scenario: 过宽的可写目录被拒绝
- **WHEN** `extra_read_write` 包含 `"/"`
- **THEN** 配置校验返回错误，系统拒绝启动

#### Scenario: 非绝对路径被拒绝
- **WHEN** 某额外挂载 `host` 为相对路径 `data/models`
- **THEN** 配置校验返回错误，系统拒绝启动

#### Scenario: 挂载目标冲突不变量被拒绝
- **WHEN** 额外挂载配置 target 为 `/workspace`
- **THEN** 配置校验返回错误，系统拒绝启动

### Requirement: 共享存储健康检查锚点不受额外目录影响
当 `storage.workspace.backend` 为 `shared` 时，`{data-dir}/data` 的共享文件系统健康检查（`CheckSharedBackend`）与 bwrap 用户命名空间映射 UID 自检（`ProbeFUSEWorkspace`）SHALL 始终以派生的 `{data-dir}/data` 为锚点，`landlock.extra_*` 与 `tools.executor.sandbox.extra_*` 不得改变或参与该锚点。

#### Scenario: 额外目录不改变共享检查锚点
- **WHEN** 共享模式下配置了 `landlock.extra_read_write` 与 `sandbox.extra_read_only`
- **THEN** 启动健康检查仍仅校验 `{data-dir}/data`
- **AND** 额外目录的存在与否不影响共享模式启动判定
