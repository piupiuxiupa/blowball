## Why

独立 Scheduler 的内部 RPC 当前强制使用 mTLS，缺少证书时无法启动，不便于受控开发和联调。需要在内网正式部署继续默认使用 mTLS 的前提下，提供显式的无 TLS 启动方式，同时保留调用者身份认证、逐方法授权和现有业务校验。

## What Changes

- 增加进程级 `security_mode`：主服务使用 `internal_rpc.security_mode`，Scheduler 使用 `rpc.security_mode`；省略时保持 `mtls`，显式 `plaintext_token` 才启用明文。每个进程的内部 RPC 监听和出站连接使用同一模式，不提供自动协商或降级。
- 保留现有 TLS 配置路径。`mtls` 仍要求证书、私钥和 CA，缺少或无效的证书仍导致初始化失败；`plaintext_token` 不加载 TLS 文件。
- 明文模式增加入站 `peer_tokens` 和出站 `client_token`。服务端根据配置的 Token 绑定服务身份，不信任客户端声明的身份头，再复用现有逐方法授权。不同身份不得共享 Token，不提供默认 Token，也不复用用户 JWT、JWT 签名密钥或 LLM 凭证。
- 明文模式必须对内部 gRPC 健康检查进行 Token 认证；健康检查仍豁免 deadline 和业务方法权限。普通 RPC 继续执行 deadline、用户归属、模型、资源、执行接收和启动授权检查。
- 明文启动明确记录不含凭证的 WARN，说明链路未加密、服务端身份未验证。示例使用回环地址；该模式只推荐用于受控开发或隔离联调，不作为内网生产部署的推荐方案。
- 补齐内网 mTLS 的 CA 信任、客户端 CN、服务端 SAN、证书用途、只读挂载和配置映射指引。服务启动时不自动生成证书或临时 CA。
- 不修改公开 HTTP/proto 业务字段、数据库结构、调度算法、执行凭证、幂等提交或用户工作空间权限；不新增匿名 RPC、Token 签发服务、热轮换、IP 信任规则或逐端点混合模式。

## Capabilities

### New Capabilities

无；本变更扩展已有内部 RPC 和服务角色能力。

### Modified Capabilities

- `scheduled-task-service`：将“所有内部 RPC 必须使用 mTLS”调整为默认 mTLS、显式明文服务 Token；定义两种模式下的认证、授权、健康检查及凭证隔离要求。此能力的前置基线由已完成的 `extract-scheduler-service` 提供。
- `service-roles`：定义主服务与 Scheduler 的进程级模式、入站/出站凭证配置、关闭内部 RPC 时的兼容行为、启动告警和内网 mTLS 部署指引。

## Impact

- 代码：`internal/rpcx/` 的凭据选择和认证中间件；`internal/config/config.go`、`services/scheduler/internal/config/config.go` 的模式与凭证校验；`cmd/blowball/serve.go`、`services/scheduler/cmd/scheduler/main.go` 的监听与客户端接线。
- 测试：两种模式的配置、传输、服务身份、逐方法授权、健康检查、错误码和任务链路回归；继续检查凭证不进入业务 proto、模型上下文或日志，不扩大工具环境变量白名单。
- 配置与文档：`config.example.yaml`、`services/scheduler/config.example.yaml`、`docs/scheduled-tasks.md`。现有 mTLS 配置兼容；新增 Token 示例仅使用环境变量占位符，不提交真实凭证、证书私钥或 CA 私钥。
- API、数据和依赖：无 HTTP/proto 字段变化，无 SQL migration；复用现有 gRPC 和 Go 标准库，不新增依赖或额外服务。
- 规格依赖：`extract-scheduler-service` 已实现但尚未同步/归档，`scheduled-task-service` 目前尚未进入主规格。本提案的 delta 基于该变更的有效要求；实施前须确认前置规格已同步，或明确先同步 `extract-scheduler-service`、再同步本变更的顺序。本次生成不修改、同步或归档前置变更。
