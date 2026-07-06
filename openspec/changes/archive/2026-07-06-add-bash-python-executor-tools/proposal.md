## Why

目前 Blowball 的 Agent 只能通过 `xizhi_*` 工具读写工作区文件，无法直接执行测试、构建脚本或数据分析程序。为了让 Chongzhi 等 Agent 能够验证代码、运行自动化脚本，需要引入受控的命令执行能力。

## What Changes

- 新增内置工具 `bash` 和 `python`，允许 Agent 在工作区内执行 shell 命令和 Python 代码。
- 引入基于 [bubblewrap](https://github.com/containers/bubblewrap)（`bwrap`）的进程级沙箱，每次执行都在独立的 mount/user/pid/network namespace 中运行。
- 默认关闭网络访问（`--unshare-net`），工作目录锁定在 `data/{userID}/workspace`。
- 新增 `tools.executor` 配置节，支持按工具开启/关闭、设置超时、输出大小限制、网络开关和环境变量白名单。
- 增加命令执行审计日志，记录命令内容、退出码、输出大小和调用上下文。
- 第一版对危险命令（如 `rm`、`curl`、`wget`、`sudo`）进行静态检查并记录警告，不阻塞执行；后续可扩展为用户确认流。
- 更新 `config.example.yaml` 与 `CLAUDE.md` 文档。

## Capabilities

### New Capabilities

- `executor-tools`: 提供受 bwrap 沙箱保护的 `bash` 和 `python` 命令执行工具，包括配置、执行、超时、输出限制、环境变量过滤和审计日志。

### Modified Capabilities

- 无。工具通过现有 `tool.Registry` 注册，Agent 编排、工具注册表和 SSE 事件机制本身不修改需求，仅新增使用者。

## Impact

- 新增内部包 `internal/tool/executor/`。
- `cmd/server/main.go` 中注册新工具家族并读取 `tools.executor` 配置。
- `internal/config/config.go` 增加 `ToolsConfig.Executor` 配置结构。
- 依赖宿主机已安装 `bwrap`；若未安装，启动时检测到缺失则报错或按配置跳过注册。
- 前端无需改动，命令执行结果通过既有 `tool_result` SSE 事件返回。
- 仅影响 Linux 部署环境；macOS/Windows 开发环境不提供该工具（bwrap 不可用）。
