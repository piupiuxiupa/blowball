## Context

Blowball 当前的工具层通过 `tool.Registry` 向 Agent 暴露 `xizhi_*` 文件工具、`webfetch`、`luban_*` 等能力。Agent 可以读写工作区文件，但无法运行构建、测试或数据处理脚本。实际编码/分析流程中，运行 `go test`、`python train.py` 等命令是高频需求。

同时，命令执行是高风险能力，必须避免：
- 读取宿主敏感文件或环境变量（如 `JWT_SECRET`、`OPENAI_API_KEY`）；
- 未授权网络外连；
- 工作区越界写或持久化后门；
- 资源耗尽或僵尸进程。

宿主机已安装 `bash`、`python3` 等运行时，且部署在 Linux 服务器上，因此基于 bubblewrap 的轻量沙箱是合理选择。

## Goals / Non-Goals

**Goals:**
- 提供 `bash` 和 `python` 两个 Agent 可调用工具。
- 每次执行在 bwrap 沙箱内运行，默认无网络、无法访问工作区外文件。
- 工作目录固定为 `data/{userID}/workspace`，命令对该目录可读写。
- 可配置超时、输出大小限制、环境变量白名单。
- 记录命令执行审计日志。
- 与现有工具注册表、Agent 编排、SSE 流完全兼容。

**Non-Goals:**
- 不支持 macOS/Windows 沙箱（bwrap 不可用）。
- 不实现 Docker/containerd 方案。
- 不实现用户侧 UI 确认弹窗（第一版仅记录危险命令警告）。
- 不预装任何语言包或镜像；依赖宿主机已安装运行时。

## Decisions

### 1. 沙箱技术：bubblewrap

**选择**：使用 `bwrap` 二进制，通过 `exec.CommandContext` 调用。

**理由**：
- 无守护进程、启动快，符合 Blowball 单次请求单次执行的模型；
- 利用 Linux user/mount/pid/network namespace，隔离力度足够；
- 与项目已有的 Landlock 文件沙箱互补（Landlock 限主进程文件访问，bwrap 限子进程）。

**替代方案**：Docker。拒绝原因：需要 dockerd、镜像管理、启动延迟高，与“用宿主机已安装运行时”冲突。

### 2. 工具形态：两个独立工具

**选择**：`bash` 接受 `command` 字符串；`python` 接受 `code` 字符串或 `file` 路径。

**理由**：
- 与现有工具命名风格一致；
- LLM 对 `bash` / `python` 有明确心智模型；
- 参数模式差异大，合并为一个 `execute` 工具会让 schema 复杂。

### 3. 网络默认关闭

**选择**：配置 `network: false` 时调用 `--unshare-net`。

**理由**：用户声明包从内部仓库安装，但运行时调用通常不需要联网；默认最小权限降低风险。

### 4. 环境变量白名单

**选择**：仅传递 `allowed_env_patterns` 匹配的环境变量，默认 `PATH`、`HOME`、`LANG`、`USER`、`PYTHON*`、`TERM`。

**理由**：Blowball 主进程持有数据库 DSN、JWT secret、OpenAI key，必须防止子进程通过 `/proc/self/environ` 或环境继承读取。

### 5. 危险命令仅警告不阻塞

**选择**：第一版对 `rm`、`curl`、`wget`、`sudo`、`sshd` 等关键词检测并在日志中 warn，不返回错误。

**理由**：过度拦截会误伤正常命令（如 `rm -f build/`），先通过审计日志观察，后续再决定是否加确认流。

### 6. 工作区绑定方式

**选择**：`--bind {workspaceRoot} /workspace --chdir /workspace`。

**理由**：命令内所有相对路径都基于工作区，与 xizhi 文件工具路径语义一致；命令输出可直接落盘供 xizhi 读取。

## Risks / Trade-offs

- **[Risk] bwrap 未安装或 user namespace 被禁用** → **Mitigation**：启动时检测 `bwrap` 版本，缺失则工具注册失败并记录 fatal 日志；文档说明 Linux + unprivileged user namespaces 要求。
- **[Risk] 沙箱内仍可访问 `/etc/passwd` 等宿主信息** → **Mitigation**：/etc 只读绑定，但仅暴露最小子集（如 `/etc/alternatives`、`/etc/python*`、`/etc/ssl`），后续可细化。
- **[Risk] 命令 CPU/内存耗尽** → **Mitigation**：Go 侧 `CommandContext` 超时 + `Setrlimit` 限制输出，后续可接入 cgroup。
- **[Risk] LLM 生成命令注入** → **Mitigation**：参数由模型以 JSON 提供，不经过 shell 解析；bash 命令直接交给 `bash -c`，仍需依赖沙箱边界。
- **[Risk] macOS 开发环境无法测试** → **Mitigation**：工具在 bwrap 不可用时跳过注册，相关测试在 Linux 运行并标记构建约束。

## Migration Plan

1. 更新 `config.example.yaml`，增加 `tools.executor` 示例。
2. 部署前确认目标 Linux 服务器已安装 `bubblewrap` 且 unprivileged user namespaces 可用。
3. 如需联网访问内部仓库，将对应工具 `network` 设为 `true`。
4. 如不需要该能力，保持 `enabled: false` 即可零影响升级。

## Open Questions

- 是否需要在第一版中就支持 `python` 工具的 `file` 参数，还是仅 `code` 片段即可？
- 危险命令列表和确认策略是否需要在设计阶段就定死？
- 审计日志是否需要持久化到数据库，还是仅结构化日志即可？
