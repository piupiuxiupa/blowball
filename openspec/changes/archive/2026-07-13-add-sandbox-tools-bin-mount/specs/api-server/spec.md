## MODIFIED Requirements

### Requirement: Runtime data root
系统 SHALL 从 `-d`/`--data-dir` 指定的单一运行时根目录派生四类落盘位置：每用户数据 `{data-dir}/data`、日志文件 `{data-dir}/logs`、全局 skills `{data-dir}/skills`、操作员工具目录 `{data-dir}/tools`；若根目录或所需子目录不存在，则 SHALL 在启动时创建。`{data-dir}/tools` 用于存放操作者为沙箱内 `bash`/`python`/`pip_install` 工具提供的 CLI 二进制，将在沙箱内以只读方式挂载到 `$HOME/.local/bin`。

#### Scenario: 默认根解析到当前工作目录
- **WHEN** 未指定 `-d`
- **THEN** 数据根为当前工作目录，四类路径分别解析为 `./data`、`./logs`、`./skills`、`./tools`（与历史布局一致，新增 `./logs` 与 `./tools`）

#### Scenario: 自定义根重新定位四类状态
- **WHEN** 执行 `serve -d /var/lib/blowball`
- **THEN** 每用户数据、日志、全局 skills、操作者工具分别写入 `/var/lib/blowball/data`、`/var/lib/blowball/logs`、`/var/lib/blowball/skills`、`/var/lib/blowball/tools`

#### Scenario: 自动创建缺失目录
- **WHEN** 指定的根目录或其子目录尚不存在
- **THEN** 系统在启动时创建这些目录（权限 0o755），包括 `{data-dir}/tools`

#### Scenario: 操作者工具目录始终被创建
- **WHEN** 服务启动且 `{data-dir}/tools` 尚不存在
- **THEN** 系统创建 `{data-dir}/tools`（即使其为空），以便 Landlock 规则与沙箱挂载始终可解析
- **AND** 启动布局日志中包含 `tools_dir` 字段
