## MODIFIED Requirements

### Requirement: Landlock process-level restriction
服务进程 SHALL 通过 go-landlock 在启动时限制文件访问范围。限制的目录来源于 `sandbox-directory-configuration` 配置（见 `sandbox-directory-configuration` 规格）：RW 目录为派生的运行时子目录 `{data-dir}/data`、`{data-dir}/logs`、`{data-dir}/skills` 并附加 `landlock.extra_read_write`；RO 目录为 `{data-dir}/tools` 并附加 `landlock.extra_read_only`；系统只读基线（默认 `/etc`、`/usr`、`/bin`、`/lib`、`/lib64`、`/proc`）来自 `landlock.system_read_only`，且经 `os.Stat` 守卫——缺失条目被跳过并记录告警。未配置任何字段时行为与配置化之前一致（防御性地与沙箱内 `--ro-bind` 并行）。

#### Scenario: Landlock applied on startup
- **WHEN** 服务启动且 `landlock.enabled` 未显式关闭
- **THEN** 系统应用 go-landlock 规则，进程只能读写 `{data-dir}/data`、`{data-dir}/logs`、`{data-dir}/skills`（及配置的 `extra_read_write`）下的文件，且只能只读访问 `{data-dir}/tools`（及配置的 `extra_read_only`）

#### Scenario: Write outside runtime dirs blocked by landlock
- **WHEN** 任何代码尝试写入运行时子目录与配置额外可写目录以外的位置
- **THEN** 操作系统级别拒绝，返回 permission denied 错误

#### Scenario: Tools directory is read-only under landlock
- **WHEN** 服务进程在 Landlock 应用后尝试写入 `{data-dir}/tools` 下的文件
- **THEN** 操作被拒绝（只读限制），返回 permission denied 错误
- **AND** 读取 `{data-dir}/tools` 下的文件仍然成功

#### Scenario: 缺失系统基线目录被跳过
- **WHEN** `landlock.system_read_only` 中的某目录在运行环境不存在（如 `/lib64` 在 aarch64）
- **THEN** 系统跳过对该目录的限制并记录告警，其余基线目录照常以只读放行

#### Scenario: 显式禁用 Landlock
- **WHEN** `landlock.enabled` 配置为 `false`
- **THEN** 系统不应用 go-landlock 规则，仅记录告警并依赖应用层路径校验
