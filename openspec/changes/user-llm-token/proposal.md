## Why

当前所有 LLM 调用共用启动时由 `openai.api_key` 构造的进程级客户端：用户无法使用自己在共享网关上的 token（额度、计费与模型权限各不相同），运营方也无法把模型成本归属到具体用户。需要一个按用户配置 LLM token 的能力，未配置时保持现有全局行为。

## What Changes

- 新增 MySQL 表 `user_llm_credentials`（每用户至多一行），明文保存用户配置的 LLM API token，通过迁移 `018_user_llm_credentials.sql` 创建。
- 新增用户级 token 管理 API：
  - `GET /api/v1/me/llm-token`：仅返回 `{configured, masked_key}`，永不返回 token 本体；
  - `PUT /api/v1/me/llm-token`：保存（去首尾空白、非空、长度上限校验后的）token，幂等覆盖旧值；
  - `DELETE /api/v1/me/llm-token`：删除用户 token，回退全局 `openai.api_key`。
- 新增按用户的 LLM 客户端解析（`ClientResolver`）：用户已配置 token 时，以全局 `openai.base_url` + 用户 token 构造 `OpenAIClient`（按 userID 缓存，基数=用户数、不淘汰）；未配置时返回现有全局客户端，行为逐字节不变。
- 所有可归属到用户的 LLM 调用统一走 resolver：主对话（Confucius/SubAgent）、会话标题生成、上下文压缩摘要、webfetch 大文档摘要。
- 用户 token 仅覆盖 `api_key` 一个维度：`openai.base_url`、`openai.models` 目录、默认模型、reasoning effort 与所有模型元数据保持全局配置不变；`GET /api/v1/models` 仍为静态目录回显，不请求网关。
- 用户已配置 token 但 token 无效时显式失败（现有 `agent_error` 通道），不做静默回退到全局 `api_key`；"未配置 → 走 config" 是唯一回退路径。
- 更新 `api/openapi.yaml`，补齐新端点契约与安全语义。

## Capabilities

### New Capabilities

- `user-llm-token`：定义 per-user LLM token 的 MySQL 存储、管理 API、客户端解析与回退语义、全调用点覆盖、显式失败与 token 保密不变量。

### Modified Capabilities

- 无。模型目录、模型选择、标题/压缩/webfetch 行为语义不变，仅凭证来源变化，由新 capability 自包含描述。

## Impact

- **数据库迁移**：`migrations/018_user_llm_credentials.sql` 新建 `user_llm_credentials` 表（`user_id` 主键、明文 `api_key`、时间戳）。
- **存储层**：`internal/model`、`internal/store/mysql` 新增凭据读写（get/upsert/delete）。
- **Agent 层**：`internal/agent` 新增按 userID 的客户端解析与缓存；orchestrator 的 `Build`（已持有 userID）改为从 resolver 取 client。
- **服务/Handler 层**：标题生成、压缩摘要、webfetch digester 的调用链补传 userID 并经 resolver 取 client；新增 token 管理 handler 与路由（api/all 角色注册，agent 角色不注册）。
- **安全**：token 永不出现在 API 响应（GET 仅回 `configured` 与脱敏尾缀）、永不写日志、不进 raw capture；MySQL 明文存储为已接受的取舍。
- **兼容性**：未配置 token 的用户行为不变；`openai.api_key` 仍是 agent/all 角色启动的必填项（兜底凭证）；现有 SSE/事件/持久化协议零变化。
