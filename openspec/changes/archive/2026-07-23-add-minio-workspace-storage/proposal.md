## Why

工作空间文件（以及 per-user 的 sessions 暖层、skills）目前只存在于**每个进程本地磁盘** `{data-dir}/data/{userID}/`。这带来两个已确认的痛点：

1. **无法多节点共享**：`api`/`agent` 角色一旦拆到不同机器（见 `service-roles`），同一用户在节点 A 上传/编辑的文件，节点 B 上的 agent 看不到——工作空间成了单节点附属物，阻碍横向扩展。
2. **无容灾备份**：本地磁盘损坏即丢失全部用户工作空间，没有对象存储级的副本/版本/生命周期保护。

目标是用 **MinIO** 做工作空间存储后端，获得多节点共享 + 容灾备份。但工作空间不仅被 HTTP 层按路径读写，还被 **executor 沙箱**（`bwrap --bind {workspace} /workspace`）当成真实 POSIX 目录使用——agent 在沙箱里跑 `bash`/`python`/`pip install`（向 `/workspace/.pip` 写成千上万小文件）、OnlyOffice 回调用 `os.Rename` 原子落盘、`xizhi_modify_file` 读改写、`validatePath` 靠 `EvalSymlinks` 防符号链接逃逸。这些都需要**真 POSIX 语义**，对象存储原生 API 给不了。

## What Changes

- **存储后端引入 JuiceFS（MinIO 支撑的 POSIX 文件系统）**：operator 用 systemd 在 `{data-dir}/data` 挂载 JuiceFS（数据块存 MinIO，元数据存**专用** Redis/MySQL）。整个 per-user 数据子树（sessions 暖层 + workspace + per-user skills）透明地落在 MinIO 上。**所有 POSIX 文件操作保持不变**——`bwrap --bind`、6 个 `xizhi_*` 工具、`validatePath` 符号链接防御、`onlyOfficePersist` 原子 `rename` 一行不改。这是选 JuiceFS 而非 s3fs / 直连 S3 API 的核心理由。
- **新增配置项 `storage.workspace.backend`（`local` 默认 ｜ `shared`）**：默认 `local`，零行为变化；`shared` 启用启动期健康检查与共享语义声明。
- **启动期健康检查（`shared` 模式）**：校验 `{data-dir}/data` 是预期的共享文件系统（可写 + `fstype` 为 `fuse`/`juicefs`）；若看起来仍是本地目录则**告警并拒绝启动**——防止「某节点忘记挂载 → 静默写本地 → 跨节点数据分叉」这一最隐蔽的运维事故。executor 启用时另跑一次 trivial `bwrap` 自检，早暴露 `--allow-other` / `user_allow_other` 缺失导致的沙箱 EACCES。
- **executor 沙箱在 FUSE 上的兼容**：JuiceFS 以 `--allow-other`（需 `/etc/fuse.conf` 开启 `user_allow_other`）挂载，使 bwrap 用户命名空间内映射 uid 可访问；`bwrap.go:76` 的 `--bind` 不改。`workspace/tmp`、`workspace/.pip` 随 workspace 落在共享 FS，**跨节点共享**（一个节点 pip 装好的包，其他节点 agent 直接可用）。
- **DR / 备份**：MinIO bucket 与 JuiceFS 元数据引擎**必须一起备份/恢复**（数据块与元数据互为索引）；提供 runbook。
- **`service-roles` 数据面措辞**：「本地文件系统存储」泛化为「共享 POSIX 文件系统（默认本地；共享模式下可为 MinIO 支撑的 FUSE 挂载）」。

## Capabilities

### New Capabilities
- `workspace-shared-storage`：工作空间（及 per-user 数据）**可**由 MinIO 支撑的共享 POSIX 文件系统（JuiceFS）承载的能力——包含透明性保证（对 POSIX 文件操作零侵入）、operator 托管挂载模型、`shared` 配置与启动健康检查、bwrap 在 FUSE 上的访问前置条件（`--allow-other`）、跨节点一致性、executor `tmp`/`.pip` 的跨节点共享含义、以及 DR 备份契约。

### Modified Capabilities
- `service-roles`：「Shared runtime data root」需求中「本地文件系统存储」的措辞泛化为「共享 POSIX 文件系统存储（可为 MinIO 支撑的 FUSE 挂载）」，使数据面共享语义覆盖网络文件系统场景。

> `workspace-api`、`executor-tools`、`office-file-editing`、`xizhi-*` 的**契约层不变**——这是 JuiceFS 透明性的直接收益：路径作用域、API 响应、沙箱绑定、原子写回全部照旧。故不为它们产出 delta spec；改动集中在新增能力 + `service-roles` 措辞 + 启动健康检查。

## Impact

- **后端代码（最小）**：`internal/config/config.go` 新增 `Storage{ Workspace{ Backend string } }` 段（`local` 默认）；`cmd/blowball/serve.go` 在 `shared` 模式下于 Landlock 之前做挂载健康检查（`fstype` + 可写探针），executor 启用时追加一次 bwrap 自检。**不改** `internal/tool/xizhi/*`、`internal/tool/executor/*`（`bwrap.go`/`runner.go`）、`internal/handler/workspace.go`、`internal/store/fs/*` 的任何业务逻辑。
- **配置**：`config.example.yaml` 新增 `storage.workspace.backend` 示例（默认 `local`）；部署文档说明 `shared` 模式的前置（JuiceFS 挂载、`/etc/fuse.conf`、专用元数据引擎）。
- **运维（主要工作量在此）**：MinIO bucket；专用 JuiceFS 元数据引擎（独立 Redis 实例/DB，HA）；systemd mount unit 把 JuiceFS 挂到 `{data-dir}/data`（`--allow-other`，先于 blowball 启动）；`/etc/fuse.conf` 开 `user_allow_other`；数据迁移（rsync 现有本地 `{data-dir}/data` 进挂载点）；备份 runbook（bucket + 元数据引擎同快照）。
- **依赖**：无新增 Go 依赖（blowball 不内嵌 JuiceFS 客户端，挂载由 operator 的 `juicefs` 二进制完成）。
- **安全**：路径作用域（`validatePath`）、Landlock、bwrap 命名空间隔离**全部保留**；`--allow-other` 扩大了 FUSE 的可访问 uid 范围，但访问仍受 blowball 的鉴权 + 路径校验 + 沙箱约束（见 design 风险条目）。
- **平台**：JuiceFS / bwrap / Landlock 均为 Linux 能力；macOS/Windows 开发态继续用 `backend: local`，不受影响。
