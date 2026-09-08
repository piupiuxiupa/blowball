## Context

`cmd/blowball/serve.go` 的 `wireAgent` 在启动时构造唯一一个 `OpenAIClient`（`agent.NewOpenAIClientWithSink(cfg.OpenAI, rawSink)`），`base_url` 与 `api_key` 作为 openai-go request options 烤进 `openai.Client`，进程内不可变。该客户端被四处共享：orchestrator（主对话 Confucius/SubAgent）、`TitleService`（标题生成）、`CompactionService`（压缩摘要）、webfetch digester（大文档摘要）。`LLMClient` 接口只有 `StreamChat`，userID 通过 context 与 `AgentFactory.Build(ctx, workspaceRoot, skillsDir, userID, override)` 流动——主对话链路已天然携带 userID，其余三处需要补传。

## Goals / Non-Goals

**Goals**

- 用户可配置自己的网关 token；未配置用户回退全局 `openai.api_key`，行为不变。
- 同一用户的所有 LLM 调用（主对话/标题/压缩/网页摘要）使用同一 token 口径，成本可归属。
- token 保密：API 永不回显本体，日志与 raw capture 永不落 token。

**Non-Goals**

- 不支持 per-user `base_url`（不引入任意供应商端点与 SSRF 面）。
- 不支持 per-user 模型目录：不请求 `base_url/v1/models`，`GET /api/v1/models` 维持静态目录回显；模型校验仍按全局 `openai.models`。
- 不做 token 加密存储（明文入 MySQL，接受 DB 备份/泄露即泄露 token 的取舍）、不做 token 有效性预检、不做多 token/多供应商。

## Decisions

### D1. 只覆盖 api_key，base_url 与模型目录保持全局

`OpenAIConfig` 注释明确"All entries share this single gateway's base_url/api_key"——本变更把 api_key 维度变为 per-user，其余全部不动。模型名、effort、配额、压缩阈值、thinking wire family 的解析逻辑零变化；`resolveModelSelection` 仍按全局目录校验。token 无权访问所选模型时由网关返回 401/403/404，走现有错误通道显式暴露。

### D2. MySQL 明文存储，每用户一行

```sql
CREATE TABLE user_llm_credentials (
  user_id     VARCHAR(64)  NOT NULL,
  api_key     VARCHAR(512) NOT NULL,
  update_time TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  create_time TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (user_id)
);
```

- 选 MySQL 而非 `.blowball/` 文件（per-user MCP 先例）：api 角色要服务 token CRUD、agent 角色要在 LLM 调用时解析凭证，MySQL 天然跨角色/跨实例共享，不依赖 shared storage backend。
- 明文是明确取舍：不引入密钥管理复杂度；缓解靠 API 不回显 + 日志脱敏不变量。
- 不加 `user_id` 外键到 `users`：按主键隔离已足够；用户删除时凭据行可随既有用户数据清理策略处理（不在本变更强制）。

### D3. ClientResolver：实现 LLMClient 接口，按调用解析凭证

openai-go 的 `Client` 把 API key 烤在 options 里，因此按用户构造独立 `OpenAIClient`。实现形态：`ClientResolver` 本身实现 `LLMClient` 接口（`StreamChat` 内部按 ctx 解析后委托），userID 从调用 ctx 读取——`Orchestrator.Handle` 已为整个 turn 注入（子 Agent、webfetch 摘要、mid-turn 压缩 round hook 全部继承），流式 handler 在入口为标题生成与 turn-start 压缩补注入。四处调用点因此零签名变更：serve.go 把 resolver 作为原 `LLMClient` 传入即可。

凭证按调用直读 DB（每个 LLM round 一次主键 SELECT，相对多秒的补全调用可忽略），不做进程内长缓存：api 角色处理 PUT/DELETE、agent 角色解析凭证时分进程部署时，本地缓存失效无法跨进程传播，直读是唯一始终满足“token 更新/删除后后续解析使用新凭证”的方案；openai-go Client 是无状态配置壳，按调用构造等价于缓存实例。resolver 读库失败时按 LLM 调用错误显式上抛，不静默降级为全局凭证。

### D4. 四个调用点全部接入

- **主对话**：`orchestratorFactory.Build` 已有 userID，改为经 resolver 取 client 注入 Confucius/SubAgent——同一 turn 整棵 spawn 树仍共用一个客户端实例（one turn, one token）。
- **标题生成**：`TitleService.GenerateTitle` 调用链补传 userID，经 resolver 取 client。
- **压缩摘要**：CompactionService 同理。
- **webfetch digester**：已有 `workspaceFn(userID)`，摘要调用按同一 userID 经 resolver 取 client。

标题/压缩/网页摘要若沿用全局 token，会静默消耗运营方配额，与"自带 token"的成本归属目标矛盾，故全部接入。

### D5. 显式失败，不静默回退

用户已配置 token 时，token 无效/无权访问模型 → 网关错误沿现有 `agent_error`/降级路径显式暴露（标题生成本就失败降级为截断消息文本）。不回退全局 api_key：静默回退会让用户请求悄悄烧运营方额度，且排障时表象"正常"。"未配置 → 全局"是唯一回退。

### D6. 保密不变量

- GET 只返回 `{configured: bool, masked_key: "sk-***abcd" 形态}`（未配置时 `configured:false` 且无 `masked_key`）；PUT/DELETE 响应不含 token。
- token 不写任何日志字段；raw capture 只记录请求 params（不含鉴权头），不受影响。

### D7. PUT 校验最小化

去首尾空白后非空、长度 ≤ 512（与列宽一致）；不做网关预连验活（失败应发生在真实调用并保留真实错误语义），不做格式启发式（不同网关 token 形态各异）。

## Risks / Trade-offs

- **明文落库**：DB 泄露即泄露 token；接受，靠回显/日志不变量收敛暴露面。
- **缓存不淘汰**：token 更新需显式失效对应项；用户数极大时内存为每用户一个轻量 client 壳，可接受。
- **api/agent 角色分机**：CRUD 与解析都走 MySQL，无共享文件系统依赖；agent 角色 LLM 调用前多一次凭据读取（可加进程内短 TTL 缓存，首版按需直读）。
