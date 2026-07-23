# workspace-shared-storage Specification

## Purpose

定义一个配置驱动的选项（`storage.workspace.backend`），将 per-user 数据根（`{data-dir}/data`，含 `sessions/`、`workspace/`、`skills/` 子树）承载于一个共享 POSIX 文件系统（实现为 MinIO 支撑的 JuiceFS 挂载：数据块存 MinIO、元数据存专用引擎）之上，使数据面跨机器共享——`api` 与 `agent` 实例即使分布在不同的主机上，也能读写同一份 per-user 数据。共享模式下，operator 在 blowball 进程启动之前完成挂载；系统在启动期（应用 Landlock 之前）执行健康检查，校验 `{data-dir}/data` 确为预期的 FUSE 共享文件系统且可写；当 executor 工具启用时额外自检 bwrap 用户命名空间映射 UID 对挂载的访问前置条件（`--allow-other`）。所有既有的 POSIX 文件操作（`xizhi_*` 工具、`WorkspaceHandler` 端点、`fs.Store`、executor 沙箱 `--bind`、`validatePath` 符号链接防御、OnlyOffice 回调的原子 `rename`）对共享文件系统透明，行为与契约不变。本规格亦约束跨实例一致性语义与容灾备份/恢复契约（要求 MinIO bucket 与元数据引擎同时一致的快照）。

## Requirements

### Requirement: 工作空间可由共享 POSIX 文件系统承载
系统 SHALL 允许通过 `storage.workspace.backend` 配置将 per-user 数据根（`{data-dir}/data`，含 `sessions/`、`workspace/`、`skills/` 子树）承载于一个共享 POSIX 文件系统之上。当 `backend` 为 `shared` 时，该根 SHALL 由 operator 在 blowball 启动前以 MinIO 支撑的 POSIX 文件系统（实现为 JuiceFS：数据块存 MinIO、元数据存专用引擎）挂载就绪。无论 `local` 还是 `shared`，所有既有的 POSIX 文件操作（`xizhi_*` 工具、`WorkspaceHandler` 端点、`fs.Store`、executor 沙箱的 `--bind`、`xizhi.ValidatePath` 的符号链接防御、OnlyOffice 回调的原子 `rename`）SHALL 在行为与契约上保持一致——共享文件系统对它们透明。

#### Scenario: 默认本地模式零行为变化
- **WHEN** `storage.workspace.backend` 未配置或为 `local`
- **THEN** 系统以本地磁盘 `{data-dir}/data` 承载 per-user 数据，所有文件操作行为与本变更前完全一致

#### Scenario: 共享模式挂在 MinIO 支撑的 POSIX 文件系统上
- **WHEN** `storage.workspace.backend` 为 `shared` 且 operator 已将 JuiceFS 挂载到 `{data-dir}/data`
- **THEN** per-user 数据物理落于 MinIO（经 JuiceFS），而 `xizhi_read_file`/`xizhi_write_file`/`xizhi_modify_file`/`xizhi_list_files`/`xizhi_tree`/`xizhi_glob_files` 与全部 workspace HTTP 端点的请求/响应契约不变

#### Scenario: 验证符号链接防御在共享文件系统上仍然生效
- **WHEN** `backend` 为 `shared` 且攻击者构造指向工作空间外的符号链接
- **THEN** `xizhi.ValidatePath` 仍通过 `EvalSymlinks` 拒绝越界访问（返回 403/FORBIDDEN），与本地模式一致

#### Scenario: 原子写回在共享文件系统上仍然成立
- **WHEN** `backend` 为 `shared` 且 OnlyOffice 保存回调触发落盘
- **THEN** `onlyOfficePersist` 的"临时文件 + `os.Rename` 覆盖"在 JuiceFS 上保持原子语义，下载/写入失败时原文件不被破坏

### Requirement: operator 托管共享文件系统挂载
当 `storage.workspace.backend` 为 `shared` 时，共享 POSIX 文件系统的挂载 SHALL 由 operator（如 systemd mount unit）在 blowball 进程启动**之前**完成；blowball 进程自身 SHALL NOT 编排或嵌入该挂载（不调用 `juicefs`、不内嵌客户端），仅消费挂载后的路径。

#### Scenario: 挂载先于 blowball 就绪
- **WHEN** 以 `shared` 模式部署
- **THEN** systemd（或等价机制）保证 JuiceFS 挂载单元 `Before=`/`Requires=` blowball 服务单元，blowball 启动时 `{data-dir}/data` 已是挂载点

#### Scenario: blowball 不内嵌挂载逻辑
- **WHEN** 审计 blowball 二进制
- **THEN** 不存在调用 `juicefs`/`mount` 以建立共享文件系统的代码路径；挂载完全是 operator 侧职责

### Requirement: 共享模式启动健康检查
当 `storage.workspace.backend` 为 `shared` 时，系统 SHALL 在启动期（应用 Landlock 之前）校验 `{data-dir}/data` 确为预期的共享文件系统：该路径 SHALL 可写，且其文件系统类型（`fstype`）SHALL 匹配共享文件系统实现（JuiceFS 的 FUSE 类型族）。校验失败时系统 SHALL 拒绝启动（fatal）并输出修复指引。该检查 SHALL 仅在 Linux 生效；非 Linux 平台在 `shared` 模式下 SHALL 记录告警并按平台能力处理（开发态通常不使用 `shared`）。

#### Scenario: 共享挂载就绪则正常启动
- **WHEN** `backend` 为 `shared` 且 `{data-dir}/data` 挂载为 JuiceFS（FUSE）且可写
- **THEN** 系统通过健康检查，继续启动

#### Scenario: 共享挂载缺失则拒绝启动
- **WHEN** `backend` 为 `shared` 但 operator 忘记挂载，导致 `{data-dir}/data` 退化为本地目录
- **THEN** 系统检测到 `fstype` 非 FUSE（或探针失败），fatal 退出并打印"请先挂载 JuiceFS"指引，避免该节点静默写本地造成跨节点数据分叉

#### Scenario: 不可写挂载则拒绝启动
- **WHEN** `backend` 为 `shared` 且挂载存在但不可写（权限/只读）
- **THEN** 系统拒绝启动并报告不可写

### Requirement: executor 沙箱在共享文件系统上的访问前置条件
当 `storage.workspace.backend` 为 `shared` 且 executor 工具启用时，共享文件系统 SHALL 以允许非挂载者 UID 访问的方式挂载（JuiceFS `--allow-other`，并要求 `/etc/fuse.conf` 开启 `user_allow_other`），使 bwrap 用户命名空间内映射 UID 能读写绑定进 `/workspace` 的工作空间。系统 SHALL 在 executor 启用且 `backend` 为 `shared` 时，启动期执行一次轻量 `bwrap` 自检（绑定工作空间子目录并完成一次写/删探针）以提前暴露该前置条件缺失。

#### Scenario: 沙箱可访问共享工作空间
- **WHEN** `backend` 为 `shared`、executor 启用，且 JuiceFS 以 `--allow-other` 挂载、`user_allow_other` 已开启
- **THEN** agent 调用 `bash`/`python` 时，bwrap 内对 `/workspace` 的读写正常，不出现 EACCES

#### Scenario: allow_other 缺失被启动自检捕获
- **WHEN** `backend` 为 `shared`、executor 启用，但挂载未带 `--allow-other` 或未开 `user_allow_other`
- **THEN** 启动期 bwrap 自检失败（探针写入 EACCES），系统 fatal 退出并提示检查 FUSE 访问选项，而非等到运行期 agent 调用才报错

### Requirement: 多节点跨实例一致性
当多个 `api`/`agent` 角色实例共享同一 JuiceFS 挂载（同一 MinIO bucket + 同一元数据引擎）时，一个实例对工作空间的写 SHALL 对其他实例可读（共享文件系统提供 close-to-open 一致性）。会话/消息读取 SHALL 继续在命中 Redis 缓存前对 MySQL 重新校验归属，使跨实例的陈旧缓存条目（如某实例删除会话后另一实例的 Redis 缓存）被安全地视为 not-found，直至 TTL 过期。

#### Scenario: 跨实例工作空间写立即对其他实例可见
- **WHEN** 实例 A 的 agent 经 `xizhi_write_file` 写入文件，实例 B 共享同一挂载
- **THEN** 实例 B 的 `xizhi_read_file` 随即可读到该文件内容

#### Scenario: 跨实例删除不返回陈旧数据
- **WHEN** 实例 A 删除某会话，实例 B 的 Redis 仍缓存该会话历史
- **THEN** 实例 B 读取该会话历史时，经 MySQL 归属校验判定为已删除，返回 not-found（与单节点删除语义一致）

### Requirement: executor tmp 与 pip 产物跨节点共享
当 `storage.workspace.backend` 为 `shared` 时，executor 创建于工作空间内的 `tmp/` 与 `.pip/` 子目录（`runner.go:52-60`、`pipTargetPath`）SHALL 随工作空间落于共享文件系统，从而跨节点共享：一个节点 agent 经 `pip_install` 安装的包，其他节点 agent 的 `python` 工具 SHALL 能在无需重新安装的情况下导入（`PYTHONPATH=/workspace/.pip` 指向共享路径）。

#### Scenario: 跨节点复用已安装的 Python 包
- **WHEN** 节点 A 的 agent 调用 `pip_install` 安装 `numpy` 到共享 `/workspace/.pip`
- **AND** 节点 B 的 agent 随后调用 `python` 执行 `import numpy`
- **THEN** 节点 B 的导入成功（包来自共享 `.pip`），无需在节点 B 再次安装

#### Scenario: 跨节点临时文件可见
- **WHEN** 节点 A 的 agent 经 `bash` 写入 `/tmp/hello.txt`（映射到共享 `workspace/tmp/hello.txt`）
- **THEN** 节点 B 的 `xizhi_read_file`（路径 `tmp/hello.txt`）可读到该内容

### Requirement: 容灾备份与恢复契约
当 `storage.workspace.backend` 为 `shared` 时，per-user 数据的容灾 SHALL 基于「MinIO 数据 bucket」与「JuiceFS 元数据引擎」**同时一致的快照**：单独备份 bucket 而不同步元数据（或反之）SHALL 被视为无效备份，无法恢复。系统文档（runbook）SHALL 明确此约束与恢复顺序（先恢复/连接元数据引擎，再指回 MinIO bucket）。

#### Scenario: 同时备份元数据与数据 bucket
- **WHEN** 运维执行备份
- **THEN** MinIO bucket 的快照与元数据引擎的快照在时间上一致（或经 JuiceFS `snapshot`/`gc` 等机制保证可恢复），二者作为一组保存

#### Scenario: 仅备份 bucket 视为无效
- **WHEN** 运维仅备份 MinIO bucket 而未备份元数据引擎
- **THEN** 该备份无法独立恢复工作空间文件系统（缺元数据索引），runbook 将其标记为不完整
