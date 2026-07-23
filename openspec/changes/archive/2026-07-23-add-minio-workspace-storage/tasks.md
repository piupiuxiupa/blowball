## 1. 配置与启动健康检查（后端）

- [x] 1.1 `internal/config/config.go` 新增 `Storage{ Workspace{ Backend string } }`（`Backend` 取值 `local`|`shared`，默认 `local`，支持 `${VAR}` 展开）；加校验：非 `local`/`shared` 报错
- [x] 1.2 `cmd/blowball/serve.go` 在 `setupRuntime` 中 Landlock 之前、`fs.New(dataDir)` 之后：若 `backend == shared`，做挂载健康检查——`{data-dir}/data` 可写探针 + `fstype` 判定（见 design 开放问题）；不满足则 fatal 退出并打印修复指引
- [x] 1.3 `serve.go`：executor 启用且 `backend == shared` 时，启动期跑一次 trivial `bwrap` 自检（`--bind {d}/data/<probeuser>/workspace /workspace` + 写/删探针文件），EACCES/失败则 fatal 并提示检查 `--allow-other`/`user_allow_other`
- [x] 1.4 `config.example.yaml` 新增 `storage.workspace.backend: local` 示例与注释（说明 `shared` 前置条件）

## 2. 透明性与跨节点一致性（回归保障）

- [x] 2.1 确认 `internal/store/fs/*`（`UserWorkspace`/`sessionPath`/`EnsureUserDirs`）、`internal/handler/workspace.go`、`internal/tool/xizhi/*`、`internal/tool/executor/*`、`internal/handler/message_stream.go:151` **无逻辑改动**（仅靠挂载点接管）；在 PR 说明里列出"零改动"清单以供 review
- [x] 2.2 跨节点一致性回归：两实例共享同一 JuiceFS 挂载——节点 A `xizhi_write_file` 后，节点 B `xizhi_read_file` 立即读到（close-to-open）；补充集成测试或人工验证脚本
- [x] 2.3 删除语义回归：节点 A 删除会话/文件后，节点 B 因读时对 MySQL 重新做归属校验而得到 not-found（验证既有"Redis 不清缓存但读时重校验"在跨节点下仍成立）

## 3. executor 在 FUSE 上的兼容验证

- [x] 3.1 实测 `bwrap --bind {JuiceFS-backed workspace} /workspace` 在 `--allow-other`+`user_allow_other` 下可被映射 uid 读写（bash/python/pip 各跑一次）
- [x] 3.2 pip 装包实测：`pip_install numpy` 写入共享 `/workspace/.pip` 后，另一节点 `python` 工具直接 `import numpy` 成功（验证 PYTHONPATH + 跨节点共享）
- [x] 3.3 验证 `onlyOfficePersist` 原子 `os.Rename`（`workspace.go:806`）在 JuiceFS 上原子成立（对比 s3fs 会失败）
- [x] 3.4 验证 Landlock（`serve.go:305`）在 `{d}/data` 为 FUSE 挂载时不误拒 xizhi/executor 读写；若误拒，显式追加路径规则

## 4. 运维与部署（runbook，非代码）

- [x] 4.1 撰写部署 runbook：MinIO bucket 创建、专用元数据引擎（HA Redis）准备、`juicefs format`、systemd mount unit（`--allow-other`，`Before=/Requires=` blowball）、`/etc/fuse.conf` 的 `user_allow_other`
- [x] 4.2 撰写存量迁移 runbook：`rsync -aHAX` 现有 `{d}/data` 进 JuiceFS、校验、原子切换步骤、停写窗口
- [x] 4.3 撰写备份/恢复 runbook：bucket + 元数据引擎**同快照**；恢复顺序（元数据先行 → 指回 bucket）；JuiceFS `snapshot`/`gc` 用法
- [x] 4.4 监控项：JuiceFS 客户端健康、元数据引擎可用性、MinIO bucket 用量；告警阈值

## 5. 文档

- [x] 5.1 `CLAUDE.md` "Persistence"/"Security" 段补一句：工作空间可在 `storage.workspace.backend: shared` 下由 MinIO 支撑的 JuiceFS 承载，POSIX 操作透明；并指出 `service-roles` 数据面此时经网络 FS 共享
- [x] 5.2 `api/openapi.yaml`：工作空间端点契约**不变**（仅 storage 说明可加一句 backend 透明）；若加注释则同步前端 `npm run generate-api`
- [x] 5.3 README/部署文档：`shared` 模式前置依赖与回滚路径

## 6. 验证

- [x] 6.1 `make test` 通过（含新增的 `config` 解析测试：默认 `local`、非法值报错、`shared` 触发健康检查路径）
- [x] 6.2 `make lint` 通过
- [x] 6.3 人工 e2e（`shared` 模式，需真实 MinIO + JuiceFS + 双 blowball 实例）：
  1. 节点 A 上传文件 → 节点 B 文件列表立即可见
  2. 节点 A agent `xizhi_write_file` → 节点 B agent `xizhi_read_file` 读到
  3. 节点 A `pip_install` → 节点 B `python` import 成功
  4. 节点 A OnlyOffice 编辑保存 → 节点 B 重新打开见最新内容
  5. 杀元数据引擎 → 验证降级行为符合预期（FS 不可用，业务报错而非静默损坏）
  6. 杀某节点 JuiceFS 挂载 → blowball 启动健康检查 fatal 拒启
- [x] 6.4 人工 e2e（`local` 模式）：回归确认零行为变化（单节点本地磁盘全流程通过）
