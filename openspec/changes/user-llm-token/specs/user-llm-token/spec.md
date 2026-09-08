## ADDED Requirements

### Requirement: Per-user LLM token storage
系统 SHALL 在 MySQL `user_llm_credentials` 表中为每个用户保存至多一行 LLM API token（`user_id` 为主键），token 以明文存储。保存操作 SHALL 幂等覆盖旧值并刷新 `update_time`；删除操作 SHALL 使该用户回退到全局 `openai.api_key`，删除不存在的行 SHALL 视为成功。

#### Scenario: Save token overwrites previous value
- **WHEN** 已有 token 的用户再次调用保存接口提交新 token
- **THEN** 表中该用户仅保留新 token 一行，`update_time` 被刷新

#### Scenario: Delete falls back to global key
- **WHEN** 用户删除自己的 token 后发起对话
- **THEN** 该用户的 LLM 调用使用全局 `openai.api_key` 认证

#### Scenario: Users are isolated
- **WHEN** 用户 A 配置 token 而用户 B 未配置
- **THEN** 仅用户 A 的 LLM 调用携带用户 A 的 token；用户 B 与其他用户互不可见对方的 token

### Requirement: Token management API
系统 SHALL 提供需要鉴权的用户级 token 管理端点：`GET /api/v1/me/llm-token`、`PUT /api/v1/me/llm-token`、`DELETE /api/v1/me/llm-token`，由 `api` 与 `all` 角色注册，`agent` 角色不注册。GET SHALL 仅返回 `{configured: bool}` 与已配置时的脱敏预览（形如 `sk-***abcd` 的尾缀），SHALL NOT 返回 token 本体。PUT SHALL 对 token 去首尾空白后校验非空且长度不超过 512，否则返回 400。PUT/DELETE 的成功响应 SHALL NOT 包含 token。

#### Scenario: Get configured token returns masked preview only
- **WHEN** 已配置 token 的用户调用 GET /api/v1/me/llm-token
- **THEN** 响应为 HTTP 200，包含 `configured: true` 与脱敏尾缀，不包含 token 本体

#### Scenario: Get unconfigured token
- **WHEN** 未配置 token 的用户调用 GET /api/v1/me/llm-token
- **THEN** 响应为 HTTP 200 且 `configured: false`，不含脱敏预览

#### Scenario: Put rejects blank or oversize token
- **WHEN** PUT 提交去空白后为空的 token，或长度超过 512 的 token
- **THEN** 系统返回 HTTP 400，不修改已存储的值

#### Scenario: Unauthenticated request rejected
- **WHEN** 未携带有效 JWT 调用任一管理端点
- **THEN** 系统返回 HTTP 401

#### Scenario: Agent role does not serve token management
- **WHEN** 服务以 `agent` 角色启动
- **THEN** 三个 /api/v1/me/llm-token 端点不被注册，请求返回 404

### Requirement: Per-user client resolution with global fallback
系统 SHALL 按用户解析 LLM 客户端：用户已配置 token 时，使用全局 `openai.base_url` 与该用户 token 构造客户端；未配置时使用全局 `openai.api_key` 构造的客户端，其行为与引入本能力之前完全一致。`openai.base_url`、`openai.models` 目录、默认模型与 reasoning effort SHALL 保持全局配置，不受用户 token 影响；`GET /api/v1/models` SHALL 维持静态目录回显且不请求网关。token 更新或删除后，后续解析 SHALL 使用新凭证。

#### Scenario: Unconfigured user keeps existing behavior
- **WHEN** 未配置 token 的用户发起请求
- **THEN** 其 LLM 调用经全局 `openai.api_key` 认证，模型选择与响应行为与变更前一致

#### Scenario: Configured user hits the shared gateway with own token
- **WHEN** 已配置 token 的用户选择全局目录中的模型发起对话
- **THEN** 请求发往全局 `openai.base_url`，鉴权使用该用户 token，模型/effort 解析规则不变

#### Scenario: Token update takes effect on subsequent calls
- **WHEN** 用户更换 token 后发起下一次请求
- **THEN** LLM 调用使用新 token，不残留旧客户端凭证

#### Scenario: Model list remains static
- **WHEN** 任一用户调用 GET /api/v1/models
- **THEN** 响应仍为全局 `openai.models` 静态目录，不依赖用户 token，不请求网关的 /v1/models

### Requirement: All user-attributable LLM calls honor the user token
主对话（Confucius 与其子 Agent）、会话标题生成、上下文压缩摘要与 webfetch 大文档摘要 SHALL 统一经按用户解析的客户端发起，使同一用户的所有 LLM 调用使用同一 token 口径。同一 turn 的整棵 agent spawn 树 SHALL 共用同一个按该用户解析的客户端。

#### Scenario: Chat turn uses user token across the spawn tree
- **WHEN** 已配置 token 的用户发起一次触发子 Agent 的对话
- **THEN** Confucius 与全部子 Agent 的 LLM 调用均携带该用户 token

#### Scenario: Title generation uses user token
- **WHEN** 已配置 token 的用户的会话触发自动标题生成
- **THEN** 标题生成的 LLM 调用携带该用户 token

#### Scenario: Compaction summary uses user token
- **WHEN** 已配置 token 的用户的会话触发上下文压缩
- **THEN** 压缩摘要的 LLM 调用携带该用户 token

#### Scenario: Webfetch digestion uses user token
- **WHEN** 已配置 token 的用户的 webfetch 触发大文档模型摘要
- **THEN** 摘要的 LLM 调用携带该用户 token

### Requirement: Explicit failure without silent fallback
用户已配置 token 时，若该 token 无效或无权访问所选模型，系统 SHALL 让网关错误沿现有错误通道显式暴露（对话走 `agent_error`，标题生成按既有规则降级为消息文本截断），SHALL NOT 静默回退到全局 `openai.api_key` 重试。"未配置 token → 使用全局 api_key" SHALL 是唯一的回退路径。

#### Scenario: Invalid token surfaces an error
- **WHEN** 已配置过期 token 的用户发起对话
- **THEN** 该 turn 以网关认证错误失败并向客户端暴露错误事件，不使用全局 api_key 重试

#### Scenario: No fallback on model permission denial
- **WHEN** 已配置 token 的用户选择了其 token 无权访问的目录模型
- **THEN** 请求以网关返回的权限/模型错误显式失败，不回退全局凭证

### Requirement: Token secrecy invariants
用户 token SHALL NOT 出现在任何 API 响应体（管理端点仅回 `configured` 与脱敏尾缀）、结构化日志或 LLM raw capture 记录中。数据库列明文存储是唯一持久化形态，本能力不引入其他落盘副本。

#### Scenario: Logs never contain the token
- **WHEN** 用户保存 token 后发起对话并触发日志与 raw capture
- **THEN** 日志字段与 raw capture 记录中均不出现 token 本体

#### Scenario: Masked preview cannot reconstruct the token
- **WHEN** GET 返回脱敏预览
- **THEN** 预览仅包含固定掩码与 token 末尾少量字符，不足以还原完整 token
