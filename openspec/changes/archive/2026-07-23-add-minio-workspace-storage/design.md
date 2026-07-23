## Context

工作空间文件当前只在本地磁盘 `{data-dir}/data/{userID}/`（`fs.New(dataDir)`，`dataDir = filepath.Join(dataRoot, "data")`，`serve.go:225/288`）。它被两类语义不同的消费者使用：

- **按路径字节读写**：`WorkspaceHandler`（upload/download/content/delete/rename/onlyoffice-callback）与 6 个 `xizhi_*` 工具，全部经 `xizhi.ValidatePath`（`validate.go:71-134`，含 `EvalSymlinks` + `os.SameFile`）后做 `os.*` 调用。
- **真实 POSIX 目录**：executor 沙箱 `bwrap --bind {workspaceRoot} /workspace`（`bwrap.go:76`），agent 在内跑 `bash`/`python`/`pip install --target /workspace/.pip`（`register.go:105`），依赖 rename 原子性、随机写、目录列举。

目标：用 MinIO 做存储后端，获得**多节点共享**（`api`/`agent` 跨机）+ **容灾备份**，同时**不破坏 executor 的 POSIX 依赖**。

候选方案（探索期已评估）：
1. **直连 S3 API**：重写 `validatePath`（丢符号链接防御）、6 个 xizhi 工具、7 个 HTTP 端点、`onlyOfficePersist` 原子性，executor 仍需「拉到本地临时目录→bind→同步回」的桥——工作量最大、语义损失最多。
2. **s3fs/goofys 挂载**：POSIX 保真度低，rename 非原子（`onlyOfficePersist`、pip 安装会损坏），pip 小文件爆炸（每个 `PutObject`）。
3. **JuiceFS（MinIO 作数据后端 + 独立元数据引擎）**：真 POSIX，bwrap 那行不改，executor/pip/onlyoffice/modify_file 全部照旧，原生支持多节点并发——本变更采用。

## Goals / Non-Goals

**Goals**
- 工作空间（及 per-user sessions/skills）可由 MinIO 支撑的共享 POSIX 文件系统承载。
- 多节点 `api`/`agent` 进程看到同一份工作空间；agent 在任意节点都能读到用户在别处上传/编辑的文件。
- 容灾：MinIO 提供副本/版本/生命周期；有明确的备份/恢复路径。
- 对现有 POSIX 文件操作**零侵入**：bwrap bind、xizhi、validatePath、原子 rename 全部不动。
- 默认 `local` 模式零行为变化；开发态（macOS/Windows）不受影响。

**Non-Goals**
- presigned URL 直连 MinIO（前端/浏览器不经后端中转）——后续优化。
- OnlyOffice DocumentServer 直连 MinIO（presigned GET/PUT，省后端中转）——后续优化。
- 把 sessions 暖层 / skills 拆到与 workspace 不同的存储——本变更让整个 `{data-dir}/data` 子树共享。
- 对象存储原生重写（放弃 POSIX 的那条路）。
- JuiceFS 的多卷/多 bucket 分片、跨机房复制编排。

## Decisions

### 决策 1：采用 JuiceFS，而非 s3fs 或直连 S3 API
**选择**：JuiceFS（数据块存 MinIO，元数据存专用引擎），以 FUSE 挂载呈现为真 POSIX 文件系统。
**理由**：executor 是已启用且依赖 POSIX 的热路径（pip 装包、rename、随机写）；JuiceFS 是三者中唯一同时满足「真 POSIX（保住 bwrap/xizhi/onlyoffice 不改）」+「多节点并发原生支持」的选项。直连 S3 API 要重写安全承重墙 `validatePath` 并为 executor 造本地桥；s3fs 的非原子 rename 会让 OnlyOffice 回调与 pip 安装产生半截文件。
**代价**：MinIO 里是 JuiceFS 分块 + 元数据，**非人类可读的裸对象**；备份从「拷 bucket」变成「bucket + 元数据引擎同快照」。
**备选**：s3fs / goofys（POSIX 保真不足，否决）；直连 S3 API（工作量与语义损失最大，否决）。

### 决策 2：挂载点 = `{data-dir}/data`（整个 per-user 数据子树共享）
**选择**：JuiceFS 挂载在 `{data-dir}/data`，使 `{userID}/{sessions,workspace,skills}` 整体落在 MinIO 上。
**理由**：`fs.New(dataDir)` 已从此处派生全部 per-user 路径（`fs.go`、`user.go`），**零路径推导改动**；sessions/skills 顺带获得共享+备份，得到单一备份故事。`logs/`、全局 `skills/`、`tools/` 留在本地磁盘（非 per-user、不需共享）。
**代价**：sessions 暖层（每条消息写一次 JSON）与 per-user skills 也走网络 FS。可接受：Redis 热缓存在前，JuiceFS 本地缓存兜底，且暖层写本就 best-effort。
**备选**：仅挂 `workspace` 子树 → 要拆 `fs.Store` 路径推导（`UserWorkspace` vs `sessionPath`/`UserSkills` 分流）+ 改 `message_stream.go:151` 内联派生，改动大、收益小，否决。

### 决策 3：operator 托管挂载（systemd），blowball 不编排 JuiceFS
**选择**：operator 用 systemd unit 在 blowball 启动前把 JuiceFS 挂到 `{data-dir}/data`（`--allow-other`，`Requires=`/`After=` 保证顺序）；blowball 仅消费该路径并在 `shared` 模式做健康检查。
**理由**：blowball 不应成为文件系统编排器；systemd 提供依赖顺序、崩溃重启、干净卸载。blowball 进程内不嵌入 `juicefs` 客户端，无新增 Go 依赖。
**代价**：多一个部署单元与一份 systemd 配置。

### 决策 4：元数据引擎专用，不与 blowball 业务 Redis/MySQL 共用 keyspace
**选择**：JuiceFS 元数据引擎用独立 Redis 实例（或独立 DB index），不复用 blowball 的会话缓存 Redis。
**理由**：JuiceFS 元数据是其可用性生命线（引擎宕机 → FS 不可用）；与业务缓存耦合会互相拖累、互相影响可用性边界。生产建议元数据引擎 HA。
**代价**：多一个需 HA 的组件。

### 决策 5：`shared` 模式启动健康检查（防静默跨节点分叉）
**选择**：`storage.workspace.backend: shared` 时，启动期在 Landlock 之前校验 `{data-dir}/data` 的 `fstype` ∈ {fuse 系列} 且可写；不满足则**告警并拒绝启动**。executor 启用时追加一次 trivial `bwrap`（`--bind {d}/data /workspace` + `touch /workspace/.jfs-probe && rm`）自检，捕获 `--allow-other`/`user_allow_other` 缺失导致的 EACCES。
**理由**：「某节点忘挂 JuiceFS → `{data-dir}/data` 退化为本地目录 → 该节点静默写本地 → 与其他节点数据分叉」是最危险且最隐蔽的故障；启动期硬失败是最经济的防线。
**代价**：启动多 1～数秒（bwrap 自检）；fstype 探测需 Linux（非 Linux 跳过）。

### 决策 6：FUSE 访问前置条件显式化（`--allow-other`）
**选择**：JuiceFS 挂载带 `--allow-other`，`/etc/fuse.conf` 开启 `user_allow_other`。
**理由**：bwrap 用 `--unshare-user`（`bwrap.go:62`）建立用户命名空间，沙箱进程以**映射 uid** 运行；FUSE 默认仅允许挂载者 uid 访问 → 不开 `--allow-other` 则沙箱进程访问 `/workspace` 直接 EACCES。这是不在代码里、但必须文档化的运维硬约束。

## Risks / Trade-offs

- **[Landlock × FUSE 交互]** Landlock（`serve.go:305`，R/W 放行 `{d}/data`）是 path-based；FUSE 挂载点仍是 VFS 路径，理论上放行成立，但 Landlock 对 FUSE 操作的覆盖有内核层细微差异。→ **需实测**：`shared` 模式下确认 xizhi/executor 能正常读写不被 Landlock 误拒；若误拒，把挂载路径以独立规则显式加入 ruleset。
- **[元数据引擎成为新单点]** 元数据引擎宕机 → 工作空间不可读写（业务 Redis/MySQL 在线也无济于事）。→ 生产元数据引擎必须 HA（Redis Sentinel/Cluster 或 TiKV）；监控 + 告警。
- **[备份非裸对象]** MinIO 里是 JuiceFS 分块，单独拷 bucket 无法恢复（缺元数据）。→ 备份必须 bucket + 元数据引擎**同时一致快照**；恢复按 JuiceFS 文档先恢复元数据再指回 bucket。runbook 化。
- **[跨节点 `.pip` 并发冲突]** 两节点 agent 同时 `pip install` 冲突版本到同一用户 `.pip`。→ 现状每用户独立 `.pip`，冲突面限于单用户的并发 agent；JuiceFS 的元数据锁保证文件级一致，但包级语义冲突需用户侧规避。记录，不加锁（non-goal）。
- **[`--allow-other` 扩大可访问面]** 该选项让同机其他 uid 可访问 FUSE 挂载。→ 访问仍受 blowball 鉴权 + `validatePath` + bwrap 命名空间约束；机器本身是受控服务器（非多租户共享主机）。可接受；若强隔离需求，用独立挂载命名空间 + 受限 uid。
- **[sessions 暖层延迟]** 暖层上网络 FS 后单次写延迟略增。→ Redis 热缓存兜底读；暖层写本就 best-effort（错误被吞）。可后续把暖层留本地（非目标）。
- **[数据迁移一致性]** 切换前需把现有本地 `{d}/data` 同步进 JuiceFS。→ runbook：挂载后 `rsync` + 校验 + 切换 `backend: shared`；切换窗口内停写或加维护态。

## Migration Plan

- **纯增量**：`storage.workspace.backend` 默认 `local`，老配置与现有部署零变化。
- **新部署（`shared`）runbook**：
  1. 起 MinIO bucket；起专用元数据引擎（HA Redis）。
  2. operator 主机 `juicefs format <meta-uri> <fsname> --storage minio ...`。
  3. `/etc/fuse.conf` 加 `user_allow_other`。
  4. systemd mount unit：`juicefs mount <meta-uri> {data-dir}/data --allow-other`，`Before=blowball.service` / `Requires=`。
  5. blowball `config.yaml` 设 `storage.workspace.backend: shared`，启动。
- **存量迁移**：先挂 JuiceFS 到临时点，`rsync -aHAX {d}/data/ {mnt}/`，校验条数/校验和，原子切换（停服→改挂载点→起服），或用 JuiceFS 自带导入。
- **回滚**：`backend: local` + 卸载 JuiceFS 挂载 + 数据回 rsync 到本地 → 回到单节点本地磁盘。

## Open Questions

- 元数据引擎用**独立 Redis 实例**还是**独立 DB index**？（生产建议独立 HA 实例；开发可用 DB index。）
- 启动期 bwrap 自检是否纳入**首版**？（建议纳入：成本极低、能挡住最隐蔽的 allow_other 误配。）
- 是否标准化 JuiceFS 本地缓存目录（`--cache-dir`/`--cache-size`）写入部署文档默认值？（建议给推荐值，避免每节点自填。）
- `fstype` 健康检查的判定集合：JuiceFS 在不同内核/版本下 `statfs().f_fstypename` 可能是 `fuse.juicefs` / `fuse` / `juicefs`——需实测确定判定逻辑（含子串匹配 + 可写探针双保险）。
