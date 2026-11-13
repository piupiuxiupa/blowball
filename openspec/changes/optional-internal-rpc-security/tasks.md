# Optional Internal RPC Security Implementation Plan

> **For agentic workers:** 实施时使用 `executing-plans` 逐项执行；只有用户明确授权子代理/并行时才选择 `subagent-driven-development`。本文件是待实施计划，生成提案不等于执行其中的代码、规格同步或部署操作。

**Goal:** 默认保持 mTLS，为主服务和 Scheduler 增加显式的明文服务 Token 模式，保留服务身份、方法授权和业务校验。

**Architecture:** 公共 `rpcx` 统一校验模式、选择传输凭据并解析可信调用者身份；两个配置加载器和启动入口仅传递同一进程级模式。复用原 gRPC 服务、deadline、连接和业务层。

**Tech Stack:** 项目 Go 1.26.2、现有 gRPC/Protobuf、Go 标准库、现有 Testify/bufconn/测试 PKI；不新增依赖。

**Spec:** `design.md`、`specs/scheduled-task-service/spec.md`、`specs/service-roles/spec.md`；前置为已归档 `extract-scheduler-service` 的实现和已落位主规格。

## 1. 全局约束与前置基线

**Global Constraints**

- `security_mode` 仅有 `mtls`、`plaintext_token`；省略时 `mtls`，不自动协商或降级。
- 保留 TLS 1.3 下限、原 TLS 字段、角色/方法权限和业务错误码；不修改 HTTP/proto 业务字段、SQL、调度算法或工具沙箱默认策略。
- Token 非空、可打印 ASCII、无空白/控制字符；入站仅绑定四种现有身份，不同身份不共享 Token，无默认秘密。
- 未启用主服务内部 RPC 时不要求安全凭证；仅关闭 Scheduler 执行不能关闭其 RPC 认证。
- 每个代码组按 red → green → 验证 → scoped Conventional Commit；不提交配置秘密、证书/CA 私钥、日志或 `bin/`。
- 集成测试只使用明确配置的独立测试数据库；现有 Scheduler 集成 fixture 会重建 scheduled 表，禁止指向运行/生产数据库。

**Review Focus**

1. 旧配置、省略模式和关闭内部 RPC 时的缺失凭证不能意外改变启动行为 → 4.1、5.1。
2. 多个 authorization、非法 Token 字符和伪造身份不能产生歧义认证 → 2.1、3.1。
3. 有效 Scheduler Token 自称主服务仍不能取得任务管理权限 → 3.1、5.3。
4. 健康检查豁免 deadline 不得变成明文匿名入口 → 3.1。
5. 凭证标记不得出现在错误、启动日志、业务持久记录、模型请求或默认工具环境 → 3.1、5.3、6.1。

- [x] 1.1 核对前置代码和规格来源，记录 `scheduled-task-service` 与 `Role-scoped internal RPC listeners` 是否已同步；未同步时先取得同步操作授权，确认顺序为 `extract-scheduler-service` → 本变更，不自动归档或改写前置 delta。
- [x] 1.2 运行 `openspec validate optional-internal-rpc-security --strict` 和 `go test ./internal/rpcx ./internal/config ./services/scheduler/internal/config ./cmd/blowball ./services/scheduler/cmd/scheduler -count=1`，记录实际基线、失败和集成环境条件；区分前置问题与本变更，不顺手修复无关问题。

## 2. 公共模式与凭证校验

**Files:** 新建 `internal/rpcx/security.go`、`internal/rpcx/security_test.go`。

**Interfaces:** 产出 `SecurityMode`、`SecurityModeMTLS`、`SecurityModePlaintextToken`；`NormalizeSecurityMode(SecurityMode) (SecurityMode, error)`、`ValidatePeerTokens(map[string]string) error`、`ValidateClientToken(string) error`。身份常量复用现有定义，校验不进行 I/O 或日志输出。

- [x] 2.1 先写 `TestNormalizeSecurityMode`、`TestValidatePeerTokens`、`TestValidateClientToken` 的表驱动测试：省略/两合法模式、未知模式、空映射、未知身份、跨身份重复 Token、空值、空白、控制字符、非 ASCII 和合法 Token；错误不得含凭证标记。
- [x] 2.2 运行 `go test ./internal/rpcx -run 'Test(NormalizeSecurityMode|ValidatePeerTokens|ValidateClientToken)' -count=1`，确认当前实现尚不满足新接口/规范，排除无关环境失败。
- [x] 2.3 实现以上类型、常量和纯校验函数，默认仅归一化省略值为 `mtls`；不自动修剪 Token、不猜身份、不提供秘密默认值或认证插件注册表。
- [x] 2.4 重跑 2.2 和 `go test -race ./internal/rpcx -count=1`，确认校验用例通过、现有 RPC 测试无回归。
- [x] 2.5 仅提交本组源文件和测试，使用 `feat(rpcx): add explicit security mode validation`。

## 3. 传输选择、Token 身份与健康检查

**Files:** 修改 `internal/rpcx/client.go`、`internal/rpcx/server.go`、`internal/rpcx/server_test.go`；新建 `internal/rpcx/token.go`、`internal/rpcx/token_test.go`。

**Interfaces:** 消费第 2 组；保留 `Dial(DialOptions) (*grpc.ClientConn, error)`、`PeerIdentity(context.Context) (string, bool)`、`UnaryServerInterceptor(ServerOptions) grpc.UnaryServerInterceptor`。`DialOptions` 新增 `SecurityMode`/`ClientToken`；`ServerOptions` 新增 `SecurityMode`/`TLS`/`PeerTokens`，产出 `NewServer(ServerOptions) (*grpc.Server, error)`。Token 调用凭据和 metadata 解析为包内实现，不外露新的身份声明 API。

- [x] 3.1 先写 `TestPlaintextTokenAuthorization`、`TestPlaintextHealthAuthorization`、`TestTransportModesDoNotDowngrade`、`TestRPCCredentialsDoNotLeak`：有效主服务 Token、缺失/错误/重复/Bearer 格式、scheme 大小写、伪造角色、Scheduler 方法越权、无 deadline 普通调用、有/无 Token 健康检查、跨模式失败、无证书明文成功和 mTLS 不接受 Token 代替证书。
- [x] 3.2 运行对应 `go test ./internal/rpcx -run 'Test(Plaintext|TransportModes|RPCCredentials)' -count=1`，确认预期失败；使用 bufconn、测试 PKI 和有界 deadline，不依赖真实服务。
- [x] 3.3 实现 `Dial` 的两种凭据分支和公共 `NewServer`：默认复用 mTLS，明文只使用 insecure transport + 专用调用凭据；保留 lazy 连接、默认 deadline、原错误映射和测试 dialer，不自动重试其他协议。
- [x] 3.4 实现单值 Bearer 认证及常量时间 Token 比较，主体只来自服务端配置；复用方法权限。明文 health 先认证再豁免 deadline/方法权限，mTLS health 保留原握手验证；任何错误分支均 fail closed，响应/日志不得回显凭证。
- [x] 3.5 运行 `go test -race ./internal/rpcx -count=1`，确认新增用例与既有服务身份、缺证书、deadline、health、错误码和 proto 凭证排除测试全部通过。
- [x] 3.6 仅提交本组改动，使用 `feat(rpcx): support authenticated plaintext transport`。

## 4. 两侧配置与兼容行为

**Files:** 修改 `internal/config/config.go`、`internal/config/internal_rpc_test.go`、`services/scheduler/internal/config/config.go`、`services/scheduler/internal/config/config_test.go`。

**Interfaces:** 消费第 2 组校验；主服务新增 `internal_rpc.security_mode`/`peer_tokens`/`client_token`，Scheduler 新增 `rpc.security_mode`/`peer_tokens` 和 `main_service.client_token`。原 TLS 转换、endpoint 和调度 bounds 不变。

- [x] 4.1 先写/扩展两侧配置测试：旧配置默认 mTLS、明文无 TLS 文件、缺失环境变量、空/重复/未知身份 Token、非法 Token 字符、未知启用模式、主服务 RPC 关闭时凭证缺失、Scheduler 执行关闭仍需认证；mTLS 模式不要求 Token。
- [x] 4.2 运行 `go test ./internal/config ./services/scheduler/internal/config -count=1`，确认新配置行为尚未实现，原基线失败单独记录。
- [x] 4.3 实现字段加载、环境展开后的按模式校验和安全默认值；只在启用 RPC 时检查主服务凭证，Scheduler 始终检查 RPC。明文不触发 TLS 文件检查，mTLS 不因 Token 字段存在而改变认证，错误只显示字段/身份、不显示秘密。
- [x] 4.4 运行 `go test -race ./internal/config ./services/scheduler/internal/config -count=1`，确认所有配置回归通过，不改变原 endpoint、timeout、正值和旧调度配置迁移规则。
- [x] 4.5 仅提交本组改动，使用 `feat(config): configure optional rpc security modes`。

## 5. 启动接线与真实跨服务回归

**Files:** 修改 `cmd/blowball/serve.go`、`cmd/blowball/serve_scheduled_test.go`、`services/scheduler/cmd/scheduler/main.go`；新建 `services/scheduler/cmd/scheduler/main_test.go`、`services/scheduler/rpc_security_integration_test.go`，复用 `services/scheduler/integration_test.go` 的独立数据库/业务 fixture。

**Interfaces:** 消费第 3 组的 `NewServer`/`Dial` 与第 4 组的配置字段；只把模式和入站/出站凭证传给公共层，继续注册现有 Scheduler、control、runtime 和 health 服务。

- [x] 5.1 先写两入口接线测试：主服务监听/到 Scheduler 的连接、Scheduler 监听/到不同 API/Agent endpoint 的连接使用同一模式和正确凭证；主服务关闭 RPC 不创建实例，Scheduler 关闭执行仍认证。日志观察器验证明文 WARN 有风险/地址且无 Token，mTLS 无错误明文告警。
- [x] 5.2 运行 `go test ./cmd/blowball ./services/scheduler/cmd/scheduler -count=1`，确认新接线/告警用例预期失败，不以真实 MySQL/Redis 连接错误代替行为证明。
- [x] 5.3 新增 `TestScheduledRPCSecurityLifecycle`，在专用 `MYSQL_TEST_DSN` 测试库中为 `mtls`/`plaintext_token` 各建立真正的 gRPC 连接与现有业务适配器：创建/读取任务、提交/接收执行、权威 occurrence/启动授权和幂等提交；断言越权/归属校验未绕过，业务记录与 fake turn 请求无凭证标记。不能只使用当前进程内直接调用证明传输模式正确。
- [x] 5.4 用公共工厂替换两个入口的重复 mTLS 接线，传递进程级模式和正确 Token，添加不含秘密的明文启动 WARN；不复制认证策略，不改 HTTP、业务或调度逻辑。
- [x] 5.5 重跑入口测试，并在明确的独立测试库中执行 `go test -tags=integration ./services/scheduler -run TestScheduledRPCSecurityLifecycle -count=1 -v`；确认两模式均实际运行且通过，因 `MYSQL_TEST_DSN` 未设置的 SKIP 不算验收。
- [x] 5.6 仅提交本组源文件和测试，使用 `feat(scheduler): wire optional authenticated rpc modes`。

## 6. 配置示例、内网证书与凭证边界

**Files:** 修改 `config.example.yaml`、`services/scheduler/config.example.yaml`、`docs/scheduled-tasks.md`、`internal/tool/executor/env_test.go`；不改变 executor 生产环境策略。

**Interfaces:** 对外只说明第 4 组配置字段和第 5 组启动行为；复用现有 env 过滤测试接口，不增加自动签发/分发服务。

- [x] 6.1 先添加 `TestDefaultExecutorEnvExcludesRPCTokens`，设置示例 `MAIN_RPC_TOKEN`/`SCHEDULER_RPC_TOKEN` 并验证默认环境过滤不包含它们；运行 `go test ./internal/tool/executor -run TestDefaultExecutorEnvExcludesRPCTokens -count=1`，不通过时先调查默认策略，不扩大白名单来“修复”。
- [x] 6.2 更新默认 mTLS 示例和完整的回环明文片段；只使用 `${ENV_VAR}` 秘密占位符，解释 Token 角色映射、明文风险、双方协调切换、禁止向 executor/MCP 环境转交 Token、旧二进制回滚前恢复 mTLS。
- [x] 6.3 补齐内部 CA 的开发签发/验证命令、CN/SAN/ServerName/EKU 区分、证书用途复用条件、只读挂载和缺 `scheduler-client.crt` 排查；在临时目录验证命令/证书与配置映射，不把开发材料当作生产 CA，也不提交任何私钥或证书产物。
- [x] 6.4 验证示例能通过对应模式的配置校验，运行 `go test ./internal/tool/executor ./internal/config ./services/scheduler/internal/config -count=1`，检查文档无真实凭证或生成的证书/私钥产物。
- [x] 6.5 仅提交本组文档、示例与环境回归测试，使用 `docs(scheduler): document rpc security and internal tls setup`。

## 7. 全量验收与规格收尾

- [x] 7.1 运行 `make test`，确认完整 `go test -race ./...` 通过；记录集成测试是否实际运行，不把 SKIP 描述为通过。
- [x] 7.2 运行 `make lint`，确认 `go vet ./...` 无问题；不提交 `make build`/`make build-scheduler` 的 `bin/` 产物。
- [x] 7.3 运行 `openspec validate optional-internal-rpc-security --strict` 和 `git diff --check`，逐项核对两份 delta 的场景覆盖、配置字段/接口名一致性和实际实现；最终提交仅包含本次相关源代码、测试、文档与任务状态。
- [x] 7.4 用户要求同步/归档时先确认前置规格已经同步，再处理本变更；不覆盖或丢失原 actor/执行归属、角色隔离、无本地 fallback 场景，不在用户未要求时自动归档任一变更。

## 8. 审查修复

保留 `Health.Watch`，不通过删除健康订阅规避认证；RPC Token 必须按字符串原值展开，其他配置字段继续保持既有展开、类型和校验语义。两项修复在当前分支原位执行，分别经过 red → green → scoped commit，不包含已有 `.gitignore` 修改。

- [x] 8.1 在 `internal/rpcx/token_test.go` 添加 `TestPlaintextHealthWatchAuthorization`，验证无 deadline 的合法 Token、缺失/错误/重复 Token；确认失败后，在 `internal/rpcx/server.go` 为流式与 unary 调用共享认证、方法权限、deadline 和 panic 处理，更新健康检查规格及设计，运行 `go test -race ./internal/rpcx -count=1` 并提交 `fix(rpcx): authenticate streaming health checks`。
- [ ] 8.2 在两侧配置测试覆盖双引号、反斜杠、字面 `${VAR}`、YAML 别名及非法字符的 Token，先确认主服务复现失败；仅修正 RPC Token 的安全展开，保持其他字段语义和不回显秘密，更新配置规格及设计，运行两侧配置和入口 race 测试并提交 `fix(config): preserve literal rpc token environment values`。
- [ ] 8.3 运行 `make test`、`make lint`、`openspec validate optional-internal-rpc-security --strict` 和 `git diff --check`；如无独立 `MYSQL_TEST_DSN`，明确记录跨服务数据库场景为 SKIP，不自动创建/清理运行数据库，不执行规格同步或归档。
