## MODIFIED Requirements

### Requirement: Role-scoped internal RPC listeners

API/Agent SHALL 按角色提供独立配置的内部 RPC 能力；all 可以在同一内部监听器合并两组。内部监听 SHALL 不改变公共 HTTP 角色/端口，并遵循默认 mTLS 或显式 `plaintext_token` 模式的身份认证与原服务身份授权。主服务未启用内部 RPC 时，缺少 Scheduler endpoint SHALL NOT 阻止普通主服务启动；明确启用但配置不完整时 SHALL 拒绝 RPC 初始化，不能通过关闭认证或更换模式规避配置错误。配置完整后，Scheduler 暂时不可达 SHALL NOT 阻止普通主服务启动。旧启用内嵌调度配置 SHALL 给出明确迁移错误/指引，不能启动第二调度器。

#### Scenario: Scheduler is offline at main service startup
- **WHEN** Scheduler 暂时不可达
- **THEN** 普通主服务可启动；任务调用明确不可用，无本地 fallback

#### Scenario: Legacy embedded scheduling configuration remains
- **WHEN** 主服务仍配置旧 scheduled_tasks.enabled=true
- **THEN** 明确提示迁移且不启动内嵌实例，禁止静默双调度

#### Scenario: Disabled internal RPC preserves normal startup
- **WHEN** 主服务 `internal_rpc.enabled: false`，安全凭证或 Scheduler endpoint 未配置
- **THEN** 不创建内部监听或客户端，不校验未使用的安全凭证，普通聊天/HTTP 角色行为保持不变

#### Scenario: Explicitly enabled RPC needs complete configuration
- **WHEN** 主服务明确启用内部 RPC，但所选模式所需凭证或 endpoint 不完整
- **THEN** 配置/初始化明确失败，不跳过认证，不自动改用另一模式

## ADDED Requirements

### Requirement: Process-wide internal RPC security configuration

主服务 SHALL 使用 `internal_rpc.security_mode`，Scheduler SHALL 使用 `rpc.security_mode`，支持且仅支持 `mtls` 和 `plaintext_token`；启用的 RPC 在省略模式时 SHALL 默认 `mtls`，未知模式 SHALL 拒绝启动。该模式 SHALL 同时用于所属进程的内部监听与所有内部出站连接，不提供逐 endpoint 混合模式。mTLS SHALL 保留原 TLS 字段路径和校验；明文模式 SHALL 使用主服务 `internal_rpc.peer_tokens`/`internal_rpc.client_token`、Scheduler `rpc.peer_tokens`/`main_service.client_token`。

明文入站映射 SHALL 非空，键 SHALL 仅为现有 `blowball-scheduler`、`blowball-api`、`blowball-agent`、`blowball-main`；不同身份 SHALL NOT 共享 Token。入站和出站 Token SHALL 非空、只包含可打印 ASCII 字符且无空白/控制字符。程序 SHALL 在环境变量展开后校验，SHALL NOT 提供默认 Token。主服务关闭内部 RPC 时 SHALL 不要求这些凭证；Scheduler 仅关闭执行时 SHALL 仍校验 RPC 配置。endpoint、timeout 和调度正值校验 SHALL 保持原行为。

#### Scenario: Existing configuration keeps mutual TLS
- **WHEN** 现有有效 TLS 配置省略 `security_mode`
- **THEN** 监听和出站连接均继续使用 mTLS，不需要迁移字段

#### Scenario: Shared mode covers separate API and Agent targets
- **WHEN** Scheduler 的 API/Agent endpoint 不同且选择 `plaintext_token`
- **THEN** 两个出站连接及自身监听均使用明文 Token 模式，出站均使用 `main_service.client_token`

#### Scenario: Token configuration fails after missing environment expansion
- **WHEN** 明文模式需要的 Token 环境变量未设置，展开后为空
- **THEN** 初始化前拒绝配置，指出字段而不输出凭证

#### Scenario: Unknown or duplicate identity binding
- **WHEN** 入站映射包含未知身份，或不同身份配置相同 Token
- **THEN** 拒绝配置，不能根据映射顺序或请求头猜测身份

#### Scenario: Token is not representable as one credential value
- **WHEN** Token 含空白、控制字符或非 ASCII 字符
- **THEN** 拒绝配置，不等到 RPC 发送阶段才失败，也不自动修剪 Token

#### Scenario: Unknown enabled security mode
- **WHEN** 已启用 RPC 的 `security_mode` 为不支持的值
- **THEN** 启动失败，不默认明文或跳过授权

#### Scenario: Disabled execution still has authenticated RPC
- **WHEN** Scheduler 设置 `scheduler.execution_enabled: false` 并选择明文模式
- **THEN** 仍要求有效入站/出站 Token，任务管理和健康检查不变成匿名接口

### Requirement: Observable plaintext RPC startup

主服务和 Scheduler 成功启用 `plaintext_token` 时 SHALL 在启动日志记录 WARN，包含安全模式、监听/目标地址及“链路未加密、服务端身份未验证”的风险说明，SHALL NOT 包含 Token。配置和部署示例 SHALL 将明文模式标为受控开发/隔离联调用途，并使用回环地址；内网正式部署 SHALL 推荐 mTLS，SHALL NOT 将 Token 或内网位置描述为等价的 TLS 防护。

#### Scenario: Plaintext startup is explicit and observable
- **WHEN** 主服务或 Scheduler 以明文模式启动内部 RPC
- **THEN** 日志出现风险 WARN，不输出任何入站或出站 Token

#### Scenario: Mutual TLS startup is not mislabeled
- **WHEN** 主服务或 Scheduler 使用默认 mTLS 模式
- **THEN** 不记录明文启用告警，也不因残留 Token 字段改变传输模式

### Requirement: Internal mutual TLS deployment guidance

部署文档 SHALL 提供内网 CA 信任、客户端服务身份 CN、服务端 SAN 与 `server_name`、`clientAuth`/`serverAuth` 用途、证书/私钥路径和只读挂载的配置对应关系。文档 SHALL 区分 CN 的应用身份与 SAN 的服务端主机名验证，提供受控开发签发/验证指引和缺文件排查，不要求购买公网证书。CA 私钥 SHALL NOT 放入运行容器或仓库，新增示例 SHALL 不包含真实秘密；文档 SHALL 警告不得将 RPC Token 变量转交给 executor 环境白名单、环境字面量或 MCP 服务环境。

#### Scenario: Diagnose a missing Scheduler client certificate
- **WHEN** 部署出现 `main_service.tls` 客户端证书文件不存在的错误
- **THEN** 指引说明运行环境路径、挂载和配置映射的检查步骤，不建议关闭 CA/主机名验证或自动降级

#### Scenario: Distinguish service identity from hostname verification
- **WHEN** 部署方配置主服务和 Scheduler 的 mTLS 证书
- **THEN** 指引分别说明客户端 CN 身份、服务端 SAN/ServerName 匹配和必要的证书用途，CA 私钥不进入运行容器

#### Scenario: Restore mutual TLS before binary rollback
- **WHEN** 从支持明文模式的新版本回退到旧二进制
- **THEN** 指引要求先恢复 mTLS 配置与有效证书，再协调回退，不直接使用明文配置运行旧版本，也不修改任务数据
