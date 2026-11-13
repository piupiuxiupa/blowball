## Context

本变更基于已实现的 `extract-scheduler-service`。当前 `rpcx.Dial` 无条件构造客户端 mTLS 凭据，两个启动入口分别构造 mTLS 服务端；`PeerIdentity` 从经过 CA 验证的客户端证书读取 CN，再由 `DefaultAuthorizer` 检查方法权限。主服务和 Scheduler 的配置校验也要求 TLS 文件字段。

使用者希望内网正式部署继续使用 TLS，同时可以在没有证书的受控联调环境启动服务。关闭传输加密不能等同于关闭服务身份认证，更不能把来源 IP 或客户端声明的身份当作可信主体。

`extract-scheduler-service` 已于 2026-11-12 归档，`scheduled-task-service` 和 `Role-scoped internal RPC listeners` 的有效基线已进入 `openspec/specs/`。本变更不修改已归档的前置提案，也无需再次同步前置规格。

## Goals / Non-Goals

**Goals:**

- 旧配置省略新字段时保持 mTLS，包括 TLS 1.3 下限、客户端证书认证及现有错误语义。
- 显式选择无 TLS 时不访问证书文件，但仍认证调用者并复用方法权限和业务检查。
- 用一个进程级模式约束内部监听与出站连接，配置可校验、行为可测试。
- 保护健康检查和凭证处理；清晰区分“配置不完整”与“远端暂时不可达”。
- 给出可复现的内网 mTLS 证书和部署指引，不把开发 CA 当作生产 CA。

**Non-Goals:**

- 匿名 RPC、自动探测/降级、同端口双协议、逐端点混合模式。
- 单向 TLS 加 Token 的第三种模式、Token 签发服务、有效期管理、热轮换或通用认证插件框架。
- 依靠 IP 白名单或“内网可信”替代凭证认证。
- 修改 HTTP 角色/端口、业务 proto、SQL、调度/恢复、执行幂等、用户权限或工具沙箱默认策略。
- 自动生成运行时证书、自动同步/归档前置变更。

## Decisions

### D1. 两种显式模式，默认 mTLS

主服务从 `internal_rpc.security_mode` 读取模式；Scheduler 从 `rpc.security_mode` 读取模式。模式作用于该进程的内部 RPC 监听和所有内部出站连接，包括 Scheduler 的 API/Agent 两个目标。

| 配置值 | 监听与连接 | 配置校验 |
| --- | --- | --- |
| 省略或 `mtls` | 现有 mTLS | 要求证书、私钥、CA；维持 ServerName 和 TLS 1.3 下限 |
| `plaintext_token` | 明文 gRPC + 调用者 Token | 要求入站 Token 映射、出站 Token；不加载或验证 TLS 文件是否存在 |
| 其他值 | 不创建 RPC 实例 | 启用 RPC 时拒绝启动，不改用其他模式 |

主服务 `internal_rpc.enabled: false` 仍不创建内部监听和 Scheduler 客户端，也不要求安全凭证或 endpoint；不把未使用的安全字段变成普通聊天的启动依赖。Scheduler 的 RPC 不因 `scheduler.execution_enabled: false` 而关闭，仍需满足所选模式的配置。

明文模式可以保留旧 `tls` 字段，但它们不被使用；mTLS 模式不发送或接受 Token 作为证书替代。旧版配置无需新增字段。双方模式不匹配时连接/RPC 失败，不尝试另一种模式。

**Alternatives:** 只提供证书工具更简单，但不能满足可选无 TLS 的需求；独立 `tls.enabled` 和 `auth.enabled` 会产生匿名组合；自动依证书存在与否选择协议会掩盖配置错误并形成降级路径。采用两种绑定传输与认证的模式。

### D2. 服务端配置绑定 Token 与身份

入站 `peer_tokens` 是 `服务身份 -> Token` 的映射。身份仅允许现有 `blowball-scheduler`、`blowball-api`、`blowball-agent`、`blowball-main`。映射必须非空；Token 必须非空、由可打印 ASCII 字符组成、不含空白或控制字符，且不同身份不得使用相同 Token。出站 `client_token` 使用相同字符串规则，不提供默认值。

客户端使用 gRPC 调用凭据发送一个 `authorization: Bearer <token>` metadata 值。明文调用凭据仅在 `plaintext_token` 模式实例化，其 `RequireTransportSecurity` 返回 false；不复用要求 TLS 的用户认证凭据，不增加通用的“忽略传输安全”开关。

服务端拒绝缺失、多个 authorization 值、错误格式或不匹配的 Token。Token 使用常量时间值比较匹配配置，不能根据客户端身份头或 actor 字段选择主体；未知身份、重复 Token 在配置阶段被拒绝。成功后只将配置绑定的身份交给现有 `DefaultAuthorizer`。

```text
mtls            → 验证客户端证书链、读取 CN ─┐
                                           ├→ 原方法权限 → 原 actor/资源/执行校验
plaintext_token → 校验 Token、读取配置身份 ──┘
```

这只是统一授权入口，不表示两种模式具有相同的传输安全。明文 Token 不验证目标服务端身份，也不保护业务请求、响应或 Token 的机密性与传输完整性。

Token 首期由部署方生成，建议使用至少 32 个随机字节并编码为 Base64URL；程序不尝试推断字符串熵。替换 Token 通过协调修改配置/Secret 并重启完成，不做多版本热轮换。

配置层使用共享的 `RPCPeerTokens` YAML 映射解码器，只检查映射/字符串形状并返回不含输入值的错误；避免 YAML 库在错误类型诊断中回显截断的 Token。运行层仍接收 `map[string]string`，模式和身份/Token 值校验继续由 `rpcx` 负责，不改全局 YAML 解析或其他配置字段。

CLI 验收还校正了 Scheduler 既有默认/示例中的无效日志 sink `console` 为 `stdout`；日志格式仍为 `console`，不修改 logger 支持的 sink 或其他服务策略。该修正避免按新示例启动时在建立 RPC 前失败。

### D3. 配置保持原有结构

| 所属配置 | 新字段 | 用途 |
| --- | --- | --- |
| 主服务 `internal_rpc` | `security_mode`、`peer_tokens`、`client_token` | 统一模式、入站认证、到 Scheduler 的出站认证 |
| Scheduler `rpc` | `security_mode`、`peer_tokens` | 统一模式、入站认证 |
| Scheduler `main_service` | `client_token` | 到 API/Agent 的出站认证 |

以下是 `all` 主服务 + Scheduler 的联调片段，未展示的 endpoint、timeout 和调度字段仍使用原配置：

```yaml
# 主服务
internal_rpc:
  enabled: true
  security_mode: plaintext_token
  listen: "127.0.0.1:9092"
  scheduler_endpoint: "127.0.0.1:9091"
  peer_tokens:
    blowball-scheduler: "${SCHEDULER_RPC_TOKEN}"
  client_token: "${MAIN_RPC_TOKEN}"
```

```yaml
# Scheduler
rpc:
  security_mode: plaintext_token
  listen: "127.0.0.1:9091"
  peer_tokens:
    blowball-main: "${MAIN_RPC_TOKEN}"
main_service:
  api_endpoint: "127.0.0.1:9092"
  agent_endpoint: "127.0.0.1:9092"
  client_token: "${SCHEDULER_RPC_TOKEN}"
```

API/Agent 分开部署时，接收方分别配置实际对端身份；每个进程的出站 Token 应在对端映射为自己的角色。未配置的身份不能调用该监听器。示例不使用真实 Token 或环境变量默认秘密。

**Alternative:** 为每个 endpoint 增加独立模式和凭证组合会扩大配置面。本期只保持已有 endpoint/TLS 文件结构，新增一个进程级模式和必要的入站/出站 Token 字段。

### D4. 公共 RPC 层只增加必要的选择与认证

建议在 `internal/rpcx/security.go` 定义 `SecurityMode`、`SecurityModeMTLS`、`SecurityModePlaintextToken`，以及 `NormalizeSecurityMode(mode SecurityMode) (SecurityMode, error)`、`ValidatePeerTokens(tokens map[string]string) error`、`ValidateClientToken(token string) error`。这些函数处理省略默认值和纯配置检查，不读取文件、不访问网络、不记录秘密。

保持 `Dial(opts DialOptions) (*grpc.ClientConn, error)` 的接口；`DialOptions` 新增 `SecurityMode` 和 `ClientToken`。mTLS 分支复用 `ClientTLS`；明文分支使用 gRPC insecure transport credentials 和专用调用凭据。既有连接复用、默认 deadline、lazy connection 和 `Extra` 测试选项不变。

`ServerOptions` 在原 Authorizer/RequireDeadline 基础上增加 `SecurityMode`、`TLS`、`PeerTokens`，公共 `NewServer(opts ServerOptions) (*grpc.Server, error)` 校验配置并选择传输凭据、安装 unary 与 stream 中间件。两种中间件共享认证、方法权限和 deadline 检查，并分别包含 panic 处理。两个启动入口只传配置、注册原有服务，不复制 Token 认证逻辑。

保留 `PeerIdentity` 的 mTLS 含义；在中间件中按模式选择证书身份或 Token 身份。未识别模式、无凭证或空身份均 fail closed，不能把错误分支视为跳过授权。业务契约仍为 unary，但已注册的标准健康服务包含服务端流式 `Watch`，因此其认证也必须经过 stream 中间件；不新增业务流式契约。

### D5. 健康检查、错误和凭证边界

- mTLS 健康检查继续通过传输握手要求有效客户端证书；不新增业务方法白名单或 deadline 要求。
- 明文健康检查先认证 Token，再豁免 deadline 和业务方法权限；公共 HTTP `/healthz` 不受影响。
- 普通调用缺失/错误凭证返回 `UNAUTHENTICATED`，合法主体方法越权返回 `PERMISSION_DENIED`，缺 deadline 返回既有 `INVALID_ARGUMENT`，远端不可达或协议不匹配返回传输 `UNAVAILABLE`。业务校验错误继续使用原 `rpcx` 映射，不承诺多个错误同时存在时的检查优先级。
- 配置/证书错误是本地初始化错误，必须给出字段或文件路径但不能输出 Token。网络离线不变成主动拨号的启动阻塞，也不变成本地调度 fallback。
- Token 只存在于进程配置和认证 metadata；新增代码不得把它放入业务 proto、存储、SSE、模型/子 Agent 上下文或工具参数，不记录完整凭据配置、metadata 或认证失败的原始 Token。
- 不扩大 executor 的环境白名单，不自动注入 Token 到工具环境。部署指引禁止把 Token 环境变量放入 `allowed_env_patterns`、executor `env` 或 MCP 服务环境；默认 bwrap 清环境/白名单行为保持不变。
- 启用明文时主服务与 Scheduler 各记录启动 WARN，包含模式、监听/目标地址及“未加密、服务端身份未验证”的风险说明，不包含凭证。

### D6. 内网正式部署继续 mTLS

保留 `internal_rpc.tls`、`internal_rpc.scheduler_tls`、`rpc.tls` 和 `main_service.tls`。部署文档提供以下可检查的映射：

| 用途 | 身份/校验 |
| --- | --- |
| Scheduler 出站客户端证书 | CN 为 `blowball-scheduler`，用途包含 `clientAuth` |
| 主服务出站客户端证书 | CN 为实际 `blowball-main`/`blowball-api`/`blowball-agent`，用途包含 `clientAuth` |
| 内部监听服务端证书 | SAN 匹配调用方 `server_name`，用途包含 `serverAuth` |
| 双方信任 | `ca_file` 包含签发对端证书的可信 CA |

CN 的应用身份映射不能代替服务端 SAN 主机名校验。受控开发可用同时包含 `clientAuth`/`serverAuth` 的一份角色证书复用两侧用途；正式部署可以继续使用分离的客户端/服务端密钥。

文档给出开发签发/验证命令、缺少 `scheduler-client.crt` 时的路径排查和挂载示例。CA 私钥不进入运行容器或仓库，服务私钥只授予必要读取权限，运行挂载只读；生产 CA 的签发/生命周期由部署方维护。启动程序不自动生成材料。

### D7. 规格基线和验证

本变更只修改已落位主规格中的 `Versioned and authenticated internal RPC` 和 `Role-scoped internal RPC listeners` 的安全要求，并增加模式、Token、健康检查、凭证、告警和证书指引要求。完整保留原 actor、资源、执行归属、角色隔离和无本地 fallback 场景。

实施前已核对前置代码和规格均已落位。最终同步本变更仍需用户授权，不能直接修改已归档的前置 delta 来让两者“同时正确”。

测试优先复用 `rpcx` 的 bufconn/测试 PKI 和配置临时 YAML。覆盖两种模式的合法调用、凭证缺失/伪造、方法越权、健康检查、deadline、协议不匹配和凭证不泄露；任务管理/执行回归仍使用现有服务，不改调度逻辑来适配协议。

## Risks / Trade-offs

- [明文 Token 被窃听、重放，或目标被冒充] → 仅推荐受控联调，使用回环/隔离网络，明确 WARN；内网正式部署继续 mTLS，不宣称 Token 等价于 mTLS。
- [不同角色共享 Token 导致主体混淆] → 入站绑定已知身份，拒绝重复 Token，不信任声明的角色；拆分角色时各用独立凭据。
- [两端模式不同造成不可用] → 不自动协商，协调切换；测试跨模式失败，保留明确的不可用状态和既有幂等执行/对账。
- [环境白名单或工具配置泄露秘密] → 不扩大默认白名单、不注入凭证，文档明确禁止转交 RPC Token；不把部署方显式暴露秘密误称为程序可自动兜底。
- [前置旧规格覆盖新要求] → 记录前置基线和同步顺序；生成本提案时不同步或归档任何旧变更。
- [CN、SAN 或证书用途配置错误] → 分开解释应用身份与主机名验证，提供验证命令和只读挂载示例；不关闭 CA/主机名验证。

## Migration Plan

1. 使用已确认落位的前置规格按任务实施和验证，不重复同步前置变更。
2. 部署新版时保持默认 `mtls`，既有证书配置和旧节点可继续工作；不需要数据库迁移。
3. 联调需要明文时，先准备双方不同的 Token 和对应角色映射，再协调切换双方模式并重启；检查启动 WARN、认证健康检查、任务管理和执行。
4. 内网正式部署始终配置/保留 mTLS；明文配置不能作为证书缺失时的自动容错策略。
5. 回滚到旧二进制前，先恢复 `mtls`、配齐有效证书并协调重启；移除明文 Token 配置/环境变量。旧二进制不支持明文模式，不直接用明文配置回滚，也不改已有任务数据。

## Open Questions

没有阻塞性的功能或协议选择。实际内网 DNS/SAN、CA 和角色拆分由部署方填写；前置规格已经核对，本变更不替用户执行同步/归档。
