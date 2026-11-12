## MODIFIED Requirements

### Requirement: Versioned and authenticated internal RPC

系统 SHALL 提供版本化的任务管理、occurrence/启动授权、主服务 actor/模型/资源/历史及 Agent 执行 RPC。每个普通 RPC SHALL 有 deadline、经所选安全模式认证的调用者身份与每方法的服务身份授权。内部 RPC SHALL 默认使用 mTLS；只有显式配置 `plaintext_token` 时 SHALL 使用明文传输及服务 Token 认证，SHALL NOT 提供匿名模式或因 TLS 失败而降级。actor SHALL 来自可信 API/Agent 的鉴权上下文；客户端提供的内部身份头 SHALL NOT 被信任。执行端 SHALL 获取权威 occurrence 并重查用户、来源和模型权限，SHALL NOT 将提交体当作任意代理执行授权。

#### Scenario: Reject a forged internal request
- **WHEN** 外部客户端或未授权服务调用内部 RPC，或伪造 actor 元数据
- **THEN** 请求在业务操作前被拒绝，不读取用户文件或启动 Agent

#### Scenario: Verify execution ownership at the receiver
- **WHEN** 提交参数中的用户、配置 fingerprint 或资源引用与权威 occurrence 不符
- **THEN** Runtime 拒绝接收，不创建运行或修改他人的数据

#### Scenario: Preserve method authorization in plaintext mode
- **WHEN** 使用绑定到 `blowball-scheduler` 的有效 Token 调用只允许主服务身份使用的任务管理方法
- **THEN** 返回 `PERMISSION_DENIED`，不执行任务管理操作

## ADDED Requirements

### Requirement: Explicit authenticated RPC transport selection

系统 SHALL 在未配置安全模式时使用 `mtls`，保留 CA/主机名验证、客户端证书要求和 TLS 1.3 下限。显式 `plaintext_token` 模式 SHALL 不加载 TLS 文件，并在无证书文件但其他配置有效时支持正常 RPC。每个连接 SHALL 仅使用所配置的模式；缺证书、握手失败、Token 失败或协议不匹配 SHALL NOT 触发自动切换、证书生成、匿名调用或本地调度 fallback。

#### Scenario: Missing certificate in default mode
- **WHEN** 省略安全模式且所需证书、私钥或 CA 文件缺失或无效
- **THEN** 本地 RPC 初始化失败并说明字段或文件路径，不创建明文替代连接

#### Scenario: Plaintext mode without TLS files
- **WHEN** 双方显式选择 `plaintext_token`，配置有效 Token，且 TLS 路径指向不存在的文件
- **THEN** 服务不读取这些 TLS 文件，合法 RPC 可以正常完成

#### Scenario: No protocol downgrade between modes
- **WHEN** 客户端和目标服务端分别使用不同的安全模式
- **THEN** 连接或 RPC 明确失败，不尝试另一模式，不进入业务处理

### Requirement: Token-bound caller identity

明文模式的客户端 SHALL 通过单个 `authorization: Bearer <token>` metadata 值提供出站 Token。服务端 SHALL 使用自己的 `peer_tokens` 配置与常量时间 Token 值比较认证调用者，只将匹配项绑定的服务身份交给现有方法权限表。服务端 SHALL NOT 根据身份头、actor 或业务字段选择调用者身份，也 SHALL NOT 将有效 Token 解释为不受限的业务执行授权。缺失、重复 authorization 值、错误格式或不匹配的 Token SHALL 返回 `UNAUTHENTICATED`，不得进入业务操作；Bearer scheme SHALL 大小写不敏感。

#### Scenario: Valid token establishes the configured identity
- **WHEN** 主服务使用有效 Token 调用其身份允许的 Scheduler 方法
- **THEN** 接收方采用配置中绑定的主服务身份，继续执行原方法和业务权限检查

#### Scenario: Claimed identity cannot replace credentials
- **WHEN** 请求只携带自称 `blowball-main` 的身份头而没有有效 Token
- **THEN** 返回 `UNAUTHENTICATED`，身份头不产生授权

#### Scenario: Valid token cannot impersonate another role
- **WHEN** 请求提供 Scheduler 的有效 Token，但通过身份头或业务字段自称主服务
- **THEN** 仍按配置绑定的 Scheduler 身份授权，不提升到主服务权限

#### Scenario: Reject ambiguous or malformed authorization
- **WHEN** 请求提供多个 authorization 值、非 Bearer scheme、缺少 Token 或错误 Token
- **THEN** 返回 `UNAUTHENTICATED`，不调用业务 handler

### Requirement: Authenticated internal health and bounded calls

内部 gRPC 健康检查 SHALL 通过所选模式的认证：mTLS SHALL 保留握手验证客户端证书的要求，明文模式 SHALL 先验证有效服务 Token。认证后的健康检查 SHALL 继续豁免 deadline 和业务方法权限。普通 RPC SHALL 继续要求 deadline，复用既有错误码和调用超时；本变更 SHALL NOT 改变公共 HTTP `/healthz` 的行为。

#### Scenario: Authenticated plaintext health probe
- **WHEN** 有效 Token 的明文健康检查不携带 deadline
- **THEN** 可以返回健康状态，不因 deadline 或业务方法白名单被拒绝

#### Scenario: Anonymous plaintext health probe
- **WHEN** 明文健康检查缺失或携带错误 Token
- **THEN** 返回 `UNAUTHENTICATED`

#### Scenario: Keep mutual TLS health verification
- **WHEN** mTLS 健康检查没有有效客户端证书
- **THEN** 握手或调用失败，不因健康检查豁免而绕过证书验证

#### Scenario: Ordinary RPC still requires a deadline
- **WHEN** 已认证的普通 RPC 未携带 deadline
- **THEN** 按既有语义返回 `INVALID_ARGUMENT`，不执行业务方法

### Requirement: RPC credential isolation

Token SHALL 仅用于进程配置和认证 metadata，SHALL NOT 进入业务 proto、任务/执行快照、MySQL/Redis 业务记录、SSE、标题、模型或子 Agent 上下文、工具参数。新增代码 SHALL NOT 自动向工具环境注入 Token 或扩大环境变量白名单。配置、认证和传输错误 SHALL NOT 在日志、错误响应或调试配置输出中泄露 Token、完整凭据配置或原始认证 metadata；运行程序 SHALL NOT 自动签发 Token、证书或 CA。

#### Scenario: Failed authentication does not reveal a credential
- **WHEN** 明文请求因格式或 Token 不匹配被拒绝
- **THEN** 错误和日志不包含提供的 Token 或完整 authorization metadata

#### Scenario: Task data contains no RPC credentials
- **WHEN** 已认证调用创建任务、提交执行或返回任务历史
- **THEN** 业务消息、快照、持久记录、SSE 和模型上下文只包含原业务内容，不附带 RPC Token

#### Scenario: Tool defaults do not gain credential variables
- **WHEN** 主服务加载 RPC Token 并按默认 executor 环境策略执行工具
- **THEN** 本变更不将 Token 注入工具参数/环境，也不扩展默认环境白名单
