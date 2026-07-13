## MODIFIED Requirements

### Requirement: Landlock process-level restriction
服务进程 SHALL 通过 go-landlock 在启动时限制文件访问范围：对运行时子目录 `{data-dir}/data`、`{data-dir}/logs`、`{data-dir}/skills` 允许读写，对操作者工具目录 `{data-dir}/tools` 只允许只读（防御性地与沙箱内 `--ro-bind` 并行）。系统只读目录、`/etc`、`/usr`、`/bin`、`/lib`、`/lib64`、`/proc` 等以只读方式放行。

#### Scenario: Landlock applied on startup
- **WHEN** 服务启动
- **THEN** 系统应用 go-landlock 规则，进程只能读写 `{data-dir}/data`、`{data-dir}/logs`、`{data-dir}/skills` 下的文件，且只能只读访问 `{data-dir}/tools`

#### Scenario: Write outside runtime dirs blocked by landlock
- **WHEN** 任何代码尝试写入运行时子目录以外的位置
- **THEN** 操作系统级别拒绝，返回 permission denied 错误

#### Scenario: Tools directory is read-only under landlock
- **WHEN** 服务进程在 Landlock 应用后尝试写入 `{data-dir}/tools` 下的文件
- **THEN** 操作被拒绝（只读限制），返回 permission denied 错误
- **AND** 读取 `{data-dir}/tools` 下的文件仍然成功
