# Proposal: per-request-model

## Why

模型与思考等级目前是纯 operator 配置（`agents.<name>.model` + `thinking`/`reasoning_effort`），用户/前端不可选。实际使用中需要在同一部署下按请求切换模型（强模型攻坚、快模型闲聊）与思考深度，且上下文压缩阈值目前绑定全局单值 `openai.max_context_tokens`，无法随所选模型的窗口变化。本变更引入模型目录（config 定义模型列表与各自最大上下文）并让请求参数选择模型与思考等级。

## What Changes

- 配置新增 `openai.models` 模型目录：每项 `name`、`max_context_tokens`（正整数）、`thinking`（该模型是否支持 reasoning）；`openai.default_model` 指定缺省模型。所有模型共享现有 `openai.base_url`/`api_key` 单网关，`OpenAIClient` 不变。
- **缺省零行为变化**：未配置 `models` 时行为与现状完全一致（单一模型 = `openai.model`，上下文阈值 = `openai.max_context_tokens`，无请求参数可用）。
- `POST /messages` 请求体新增可选参数 `model` 与 `reasoning_effort`：`model` 必须在目录内（未知 → 400 `INVALID_MODEL`）；`reasoning_effort` 取值 `none | low | medium | high | xhigh | max`，对 `thinking: false` 的模型仅允许 `none`/缺省（违反 → 400）。缺省时沿用各 agent 现有配置。
- 覆盖语义：请求参数**无条件同时覆盖三个 agent**（Confucius/Chongzhi/Liang 使用同一模型与同一思考等级，保持一致；Liang 未配置 `output_schema`，无兼容例外）。
- 思考开关收敛为派生值：`thinking = model.thinking && effort != none`。`effort=none` 即关闭思考（不发 `reasoning_effort`、走 `max_tokens`+`temperature` 分支）——对齐 openai-go SDK 无独立思考开关、靠 effort 值控制的现实。
- 防御：若任一 agent 配置了 `output_schema` 且请求 effort ≠ none，请求 SHALL 400（fail fast，不静默降级）。
- **Context compaction 阈值跟随本次请求所选模型**：`0.8 × 所选模型的 max_context_tokens`（目录未配置时沿用 legacy `openai.max_context_tokens`）；`turn_usage.context_tokens` 照实存原始尺寸，turn-start 检查按**当前请求模型**的阈值比较（支持跨模型切换）；mid-turn 压缩 summary 模型跟随本 turn 所选模型。
- `turn_usage` 新增 `model` 列，记录该 turn 实际使用的模型（按模型成本核算）。
- 新增 `GET /api/v1/models`（JWT，api 分区）：返回可选模型列表（name/max_context_tokens/thinking）与 default。
- 与 turn-detach-resume 的交叉：run meta（`run:{rid}:meta`）记录本 turn 使用的 model。

## Capabilities

### New Capabilities
- `per-request-model-selection`: 模型目录配置（`openai.models` + `default_model`、零行为变化缺省）、请求参数 `model`/`reasoning_effort` 的校验与三 agent 一致覆盖、思考派生规则（`model.thinking && effort != none`）、`GET /api/v1/models` 列表端点。

### Modified Capabilities
- `agent-reasoning-configuration`: "Per-agent thinking toggle" 增加请求级覆盖与派生规则；"Configurable reasoning effort level" 增加 `none` 关闭语义与请求级覆盖（配置级枚举同步为 low/medium/high/xhigh/max，与现行代码一致）。
- `context-compaction`: 触发阈值从全局 `openai.max_context_tokens` 改为本次请求所选模型的 `max_context_tokens`（legacy 字段作为无目录时的回退）；summary 模型跟随本 turn 所选模型；turn-start 检查按当前请求模型阈值比较。
- `turn-cost-tracking`: `turn_usage` 记录本 turn 实际使用的模型（新增 `model` 列）。
- `service-roles`: `GET /api/v1/models` 归 api 角色路由分区。（注：本 delta 基于同批 turn-detach-resume 的路由变更叠加，归档顺序需 turn-detach-resume 在前。）

## Impact

- **Code**: `internal/config/config.go`（models 目录、default_model、校验）、`internal/handler/message_stream.go`（请求参数解析与校验）、`internal/agent/orchestrator.go`（`AgentFactory.Build` 增加 per-turn 模型/effort 覆盖，克隆三 agent 的 AgentConfig）、`internal/service/compaction.go`（阈值与 summary 模型按 turn 传入）、`internal/store/mysql`（turn_usage `model` 列 + 迁移 015）、`internal/handler/router.go` + 新 models handler（api 分区）、`api/openapi.yaml`。
- **Systems**: MySQL 迁移 `015_turn_usage_model.sql`（存量库需手动应用，沿用 013/014 惯例）；无 Redis 变更（run meta 的 model 字段由 turn-detach-resume 引入）。
- **Config**: `config.example.yaml` 增加注释示例；现有配置不改动即保持现状行为。
