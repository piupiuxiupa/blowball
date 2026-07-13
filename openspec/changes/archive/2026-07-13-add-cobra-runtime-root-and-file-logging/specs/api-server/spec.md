## MODIFIED Requirements

### Requirement: Structured logging
系统 SHALL 使用 zap 结构化日志，关键操作必须记录日志。日志编码格式由 `logging.format` 控制（默认 `json`，可选 `console`）；日志默认同时输出到控制台与文件（见“Log file persistence and rotation”）。

#### Scenario: Log format defaults to JSON
- **WHEN** 服务运行且未设置 `logging.format` 或将其设为 `json`
- **THEN** 日志使用 JSON 格式，包含 timestamp、level、trace_id、message 等字段

#### Scenario: Console format
- **WHEN** 配置 `logging.format: console`
- **THEN** 日志改用 console 编码格式输出，仍包含 timestamp、level、trace_id、message 等字段

#### Scenario: Key operation logging
- **WHEN** 以下操作发生时
- **THEN** 记录日志：用户登录、Agent 调用开始/结束、tool 调用、消息存储写入、错误发生

## ADDED Requirements

### Requirement: Command-line interface
系统 SHALL 通过 cobra 提供统一 CLI，包含 `serve` 与 `seed` 两个子命令；`-f`/`--config`（配置文件路径，默认 `config.yaml`）与 `-d`/`--data-dir`（运行时数据根目录，默认当前工作目录）为在根命令上定义、两个子命令均可用的持久化标志。

#### Scenario: serve 子命令启动 HTTP 服务
- **WHEN** 执行 `serve`（可选附带 `-f`/`-d`）
- **THEN** 系统按配置启动 Gin HTTP 服务，行为与原 `cmd/server` 一致（含路由注册、graceful shutdown）

#### Scenario: seed 子命令创建用户
- **WHEN** 执行 `seed -username alice`（可选附带 `-password`、`-f`、`-d` 等）
- **THEN** 系统创建用户，行为与原 `bin/seed` 一致

#### Scenario: 指定配置文件路径
- **WHEN** 执行 `serve -f /etc/blowball/config.yaml`
- **THEN** 系统从 `/etc/blowball/config.yaml` 加载配置

#### Scenario: 持久化标志默认值
- **WHEN** 未传入 `-f` 或 `-d`
- **THEN** 配置文件路径默认为当前工作目录下的 `config.yaml`，数据根目录默认为当前工作目录

#### Scenario: 无子命令或未知标志打印帮助并以非零码退出
- **WHEN** 不带任何子命令执行，或传入未定义的标志
- **THEN** 系统打印 cobra 帮助信息并以非零状态码退出，不启动服务

### Requirement: Runtime data root
系统 SHALL 从 `-d`/`--data-dir` 指定的单一运行时根目录派生三类落盘位置：每用户数据 `{data-dir}/data`、日志文件 `{data-dir}/logs`、全局 skills `{data-dir}/skills`；若根目录或所需子目录不存在，则 SHALL 在启动时创建。

#### Scenario: 默认根解析到当前工作目录
- **WHEN** 未指定 `-d`
- **THEN** 数据根为当前工作目录，三类路径分别解析为 `./data`、`./logs`、`./skills`（与历史布局一致，仅新增 `./logs`）

#### Scenario: 自定义根重新定位三类状态
- **WHEN** 执行 `serve -d /var/lib/blowball`
- **THEN** 每用户数据、日志、全局 skills 分别写入 `/var/lib/blowball/data`、`/var/lib/blowball/logs`、`/var/lib/blowball/skills`

#### Scenario: 自动创建缺失目录
- **WHEN** 指定的根目录或其子目录尚不存在
- **THEN** 系统在启动时创建这些目录（权限 0o755）

### Requirement: Log file persistence and rotation
系统 SHALL 将结构化日志写入 `{data-dir}/logs/` 下的文件，并按 `logging.file` 配置（`max_size_mb`、`max_backups`、`max_age_days`、`compress`）经 lumberjack 进行轮转；输出目标由 `logging.output`（默认同时包含控制台与文件）控制。

#### Scenario: 日志写入文件
- **WHEN** 服务运行
- **THEN** 在 `{data-dir}/logs/` 下产生日志文件并持续写入结构化日志

#### Scenario: 控制台与文件双写
- **WHEN** `logging.output` 为默认值（同时包含控制台与文件）
- **THEN** 每条日志同时出现在控制台（stderr）与 `{data-dir}/logs/` 下的文件中

#### Scenario: 按大小轮转
- **WHEN** 日志文件大小超过 `logging.file.max_size_mb`
- **THEN** 触发轮转：当前文件被重命名（并在 `compress: true` 时压缩），新日志继续写入同名新文件

#### Scenario: 限制保留备份数
- **WHEN** 轮转后保留的备份文件数超过 `logging.file.max_backups`
- **THEN** 最旧的备份文件被删除

#### Scenario: 可禁用文件输出
- **WHEN** `logging.output` 仅包含控制台（不含文件）
- **THEN** 系统不创建或写入日志文件，仅向控制台输出

#### Scenario: 日志目录自动创建
- **WHEN** 启动时 `{data-dir}/logs/` 不存在
- **THEN** 系统在初始化日志器之前创建该目录
